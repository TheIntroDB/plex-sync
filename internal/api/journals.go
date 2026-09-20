package api

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Journals lists the undo journals in a directory, oldest first.
//
// Only the most recent one is safe to revert blindly: undoing an older journal
// after a newer run would overwrite the newer changes, so callers that do not
// name a journal get this one.
func Journals(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "undo-") && strings.HasSuffix(name, ".jsonl") {
			names = append(names, filepath.Join(dir, name))
		}
	}
	sort.Strings(names)
	return names, nil
}
