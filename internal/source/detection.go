package source

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// FrameSeconds is how much audio one chromaprint frame covers.
//
// fpcalc emits roughly 8.1 frames per second, so a frame is about 0.1238 s.
// Every conversion between frames and milliseconds goes through this constant,
// and the rounding is deliberately conservative: a marker placed 100 ms late is
// harmless, one placed 5 s early is not.
const FrameSeconds = 0.1238

// Runner executes an external command and returns its standard output.
//
// It is an interface so the orchestration can be tested without ffmpeg or
// fpcalc installed, which is the normal case on a development machine.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as the current user.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Fingerprinter produces chromaprint fingerprints for a file.
type Fingerprinter struct {
	FFmpeg string
	Fpcalc string
	Runner Runner
}

// NewFingerprinter builds a fingerprinter, falling back to PATH lookups.
func NewFingerprinter(ffmpeg, fpcalc string, runner Runner) *Fingerprinter {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if fpcalc == "" {
		fpcalc = "fpcalc"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Fingerprinter{FFmpeg: ffmpeg, Fpcalc: fpcalc, Runner: runner}
}

// Available reports whether both binaries can be found.
func (f *Fingerprinter) Available() error {
	for _, name := range []string{f.FFmpeg, f.Fpcalc} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s not found on PATH: %w", name, err)
		}
	}
	return nil
}

// Fingerprint decodes a file's audio and returns its frames.
//
// Only the first windowSeconds of audio is read, and the audio is decoded to a
// temporary file rather than through the read path the media server uses, so
// this works the same whether the library is on local disk or on a mount.
func (f *Fingerprinter) Fingerprint(ctx context.Context, path string, windowSeconds int, tmpDir string) ([]uint32, float64, error) {
	if windowSeconds <= 0 {
		windowSeconds = 16
	}
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	tmp, err := os.CreateTemp(tmpDir, "plex-sync-*.wav")
	if err != nil {
		return nil, 0, fmt.Errorf("create temporary audio file: %w", err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()

	// Mono 16-bit PCM at a low sample rate: enough for chromaprint and small
	// enough that a window of audio is a few hundred kilobytes.
	args := []string{
		"-nostdin", "-v", "error",
		"-t", strconv.Itoa(windowSeconds),
		"-i", path,
		"-ac", "1", "-ar", "11025", "-f", "wav", "-y", tmpName,
	}
	if _, err := f.Runner.Run(ctx, f.FFmpeg, args...); err != nil {
		return nil, 0, fmt.Errorf("decode audio of %s: %w", filepath.Base(path), err)
	}

	out, err := f.Runner.Run(ctx, f.Fpcalc, "-raw", "-json", tmpName)
	if err != nil {
		return nil, 0, fmt.Errorf("fingerprint %s: %w", filepath.Base(path), err)
	}
	frames, duration, err := ParseFpcalc(out)
	if err != nil {
		return nil, 0, err
	}
	return frames, duration, nil
}

// ParseFpcalc reads fpcalc's JSON output.
//
// fpcalc -json prints {"duration": <seconds>, "fingerprint": "<base64 of
// little-endian uint32 frames>"}. The raw variant is used because comparing
// frames requires the values, not the compressed text fingerprint.
func ParseFpcalc(out []byte) ([]uint32, float64, error) {
	var payload struct {
		Duration    float64 `json:"duration"`
		Fingerprint string  `json:"fingerprint"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, 0, fmt.Errorf("parse fpcalc output: %w", err)
	}
	if payload.Fingerprint == "" {
		return nil, payload.Duration, errors.New("fpcalc returned no fingerprint")
	}
	raw, err := base64.StdEncoding.DecodeString(payload.Fingerprint)
	if err != nil {
		return nil, payload.Duration, fmt.Errorf("decode fingerprint: %w", err)
	}
	if len(raw)%4 != 0 {
		return nil, payload.Duration, fmt.Errorf("fingerprint length %d is not a multiple of 4", len(raw))
	}
	frames := make([]uint32, len(raw)/4)
	for i := range frames {
		frames[i] = binary.LittleEndian.Uint32(raw[i*4:])
	}
	return frames, payload.Duration, nil
}

// Match is the result of looking for a reference inside a target fingerprint.
type Match struct {
	// OffsetFrames is where the reference starts inside the target.
	OffsetFrames int
	// Distance is the mean normalised Hamming distance, 0 for identical.
	Distance float64
	// RunnerUp is the best distance at a clearly different offset. A match is
	// only believable when it beats the runner-up by a real margin, because a
	// repeated musical sting matches itself in several places.
	RunnerUp float64
	// Unique reports whether the best offset is separated from the runner-up.
	Unique bool
	// OffsetSeconds converts the frame offset into seconds.
	OffsetSeconds float64
}

// Compare finds the best alignment of reference inside target.
//
// It returns false when the reference cannot fit, so callers never have to
// reason about a partial match. The comparison is a mean normalised Hamming
// distance over 32-bit frames: identical audio gives 0, unrelated audio settles
// near 0.5.
func Compare(reference, target []uint32) (Match, bool) {
	if len(reference) == 0 || len(target) < len(reference) {
		return Match{}, false
	}
	if len(reference) == len(target) {
		d := distance(reference, target, 0)
		return Match{
			OffsetFrames:  0,
			Distance:      d,
			RunnerUp:      d,
			Unique:        true,
			OffsetSeconds: 0,
		}, true
	}

	best := Match{Distance: 1e9, RunnerUp: 1e9}
	limit := len(target) - len(reference)
	// A candidate counts as a different offset when it starts more than one
	// reference length away from the best one.
	minSeparation := len(reference)
	for offset := 0; offset <= limit; offset++ {
		d := distance(reference, target, offset)
		switch {
		case d < best.Distance:
			if best.Distance < 1e8 {
				best.RunnerUp = best.Distance
			}
			best.Distance = d
			best.OffsetFrames = offset
		case d < best.RunnerUp && abs(offset-best.OffsetFrames) >= minSeparation:
			best.RunnerUp = d
		}
	}
	best.OffsetSeconds = float64(best.OffsetFrames) * FrameSeconds
	best.Unique = best.RunnerUp-best.Distance >= 0.05
	return best, true
}

// distance is the mean normalised Hamming distance of reference and the target
// window starting at offset.
func distance(reference, target []uint32, offset int) float64 {
	var differing int
	for i, ref := range reference {
		differing += bits.OnesCount32(ref ^ target[offset+i])
	}
	return float64(differing) / float64(len(reference)*32)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// FramesForRange slices the frames covering a time range.
func FramesForRange(frames []uint32, startMS, endMS int64) []uint32 {
	start := int(float64(startMS) / 1000 / FrameSeconds)
	end := int(float64(endMS) / 1000 / FrameSeconds)
	if start < 0 {
		start = 0
	}
	if end > len(frames) {
		end = len(frames)
	}
	if start >= end || start >= len(frames) {
		return nil
	}
	return frames[start:end]
}

// Store persists fingerprints between runs.
//
// Fingerprints are keyed by file path and size, so a replaced or upgraded file
// is simply fingerprinted again instead of inheriting another release's audio.
// Each one is about six kilobytes, which is why they are kept: reading the
// audio is by far the expensive part.
type Store struct {
	dir string
}

// NewStore opens a fingerprint store under dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("fingerprint store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create fingerprint store %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

type storedFingerprint struct {
	Key       string   `json:"key"`
	Path      string   `json:"path"`
	Size      int64    `json:"size"`
	DurationS float64  `json:"duration_s"`
	Frames    []uint32 `json:"frames"`
	StoredAt  int64    `json:"stored_at"`
}

// Key builds the store key for a file path and size.
func Key(path string, size int64) string {
	sum := uint64(1469598103934665603)
	for i := 0; i < len(path); i++ {
		sum ^= uint64(path[i])
		sum *= 1099511628211
	}
	return fmt.Sprintf("%016x-%d", sum, size)
}

// Get returns a stored fingerprint, and false when there is not one.
func (s *Store) Get(key string) ([]uint32, float64, bool) {
	file, err := os.Open(s.path(key))
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = file.Close() }()

	zr, err := gzip.NewReader(file)
	if err != nil {
		return nil, 0, false
	}
	defer func() { _ = zr.Close() }()

	var stored storedFingerprint
	if err := json.NewDecoder(zr).Decode(&stored); err != nil {
		// A truncated file is not worth reporting: it is a cache miss.
		return nil, 0, false
	}
	return stored.Frames, stored.DurationS, true
}

// Put stores a fingerprint, replacing any previous entry for the key.
func (s *Store) Put(key, path string, size int64, durationS float64, frames []uint32) error {
	stored := storedFingerprint{
		Key:       key,
		Path:      path,
		Size:      size,
		DurationS: durationS,
		Frames:    frames,
		StoredAt:  time.Now().Unix(),
	}
	tmp, err := os.CreateTemp(s.dir, "fp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	zw := gzip.NewWriter(tmp)
	err = json.NewEncoder(zw).Encode(stored)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Rename last: a half-written fingerprint is never visible under its key.
	return os.Rename(tmpName, s.path(key))
}

func (s *Store) path(key string) string {
	return filepath.Join(s.dir, key+".fp.gz")
}

// Reference is a known segment in another file of the same season.
type Reference struct {
	// Path is the sibling file the fingerprint is read from.
	Path string
	// Size guards against reusing a fingerprint after the file changed.
	Size int64
	// StartMS and EndMS are the known segment, from TheIntroDB or from Plex's
	// own detection.
	StartMS int64
	EndMS   int64
	// Source names where the known segment came from, for reporting.
	Source string
}

// Detector finds segments by matching audio against a known sibling.
type Detector struct {
	Fingerprinter *Fingerprinter
	Store         *Store
	// WindowSeconds is how much of each file is decoded.
	WindowSeconds int
	// ToleranceS is the largest offset error a match may have.
	ToleranceS float64
	// CacheTTLDays controls how long a stored fingerprint stays valid.
	CacheTTLDays int
}

// IntroResult is a detected intro.
type IntroResult struct {
	StartMS int64
	EndMS   int64
	Match   Match
}

// DetectIntro looks for a known intro inside another episode of the same season.
//
// The reference is a segment another source already knows for a sibling file.
// This is our own detection, and it is deliberately the last source consulted:
// it is only asked about episodes no other source covered, and it only writes a
// marker when the match is unambiguous. A miss stays a miss; an intro is never
// invented from a partial match.
func (d *Detector) DetectIntro(
	ctx context.Context,
	targetPath string,
	targetSize int64,
	reference Reference,
	tmpDir string,
) (*IntroResult, error) {
	if d.Fingerprinter == nil || d.Store == nil {
		return nil, errors.New("detector is not configured")
	}
	refFrames, err := d.frames(ctx, reference.Path, reference.Size, tmpDir)
	if err != nil {
		return nil, err
	}
	targetFrames, err := d.frames(ctx, targetPath, targetSize, tmpDir)
	if err != nil {
		return nil, err
	}

	window := FramesForRange(refFrames, reference.StartMS, reference.EndMS)
	if len(window) < 8 {
		// Under about a second of reference audio cannot be matched reliably.
		return nil, nil
	}
	match, ok := Compare(window, targetFrames)
	if !ok || !match.Unique {
		return nil, nil
	}
	tolerance := d.ToleranceS
	if tolerance <= 0 {
		tolerance = 5
	}
	if match.Distance > 0.25 || match.OffsetSeconds > tolerance+float64(reference.EndMS-reference.StartMS)/1000 {
		return nil, nil
	}

	length := reference.EndMS - reference.StartMS
	start := int64(match.OffsetSeconds * 1000)
	if match.OffsetSeconds < 0 {
		start = 0
	}
	return &IntroResult{StartMS: start, EndMS: start + length, Match: match}, nil
}

func (d *Detector) frames(ctx context.Context, path string, size int64, tmpDir string) ([]uint32, error) {
	key := Key(path, size)
	if frames, _, ok := d.Store.Get(key); ok {
		return frames, nil
	}
	frames, duration, err := d.Fingerprinter.Fingerprint(ctx, path, d.WindowSeconds, tmpDir)
	if err != nil {
		return nil, err
	}
	if err := d.Store.Put(key, path, size, duration, frames); err != nil {
		return nil, err
	}
	return frames, nil
}

// SegmentSet wraps a detection as a segment set for the planner.
func (r *IntroResult) SegmentSet(item model.LibraryItem) model.SegmentSet {
	set := model.SegmentSet{Source: model.SourceDetection}
	if r == nil {
		return set
	}
	start, end := r.StartMS, r.EndMS
	if duration := item.BestDuration(); duration != nil && end > *duration {
		end = *duration
	}
	if end <= start {
		return set
	}
	set.Segments = append(set.Segments, model.Segment{
		Type:       model.SegmentIntro,
		StartMS:    &start,
		EndMS:      &end,
		Source:     model.SourceDetection,
		Confidence: &r.Match.Distance,
	})
	return set
}

// CopyFrames is a helper for tests and callers that need to detach frames from
// a shared buffer.
func CopyFrames(frames []uint32) []uint32 {
	out := make([]uint32, len(frames))
	copy(out, frames)
	return out
}
