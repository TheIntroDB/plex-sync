package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A malformed schedule must be caught when configuration is checked, not by a
// container sitting idle at midnight having never run.
func TestValidateRejectsABadSchedule(t *testing.T) {
	for _, expr := range []string{"", "   ", "30 7 * *", "60 7 * * *", "five * * * *"} {
		cfg := Default()
		cfg.Schedule.Cron = expr
		err := cfg.Validate()
		if err == nil {
			t.Errorf("schedule.cron %q was accepted", expr)
			continue
		}
		if !strings.Contains(err.Error(), "schedule.cron") {
			t.Errorf("schedule.cron %q: error does not name the field: %v", expr, err)
		}
	}
}

func TestValidateAcceptsTheDefaultSchedule(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default configuration does not validate: %v", err)
	}
	if cfg.Schedule.Cron != DefaultSchedule {
		t.Errorf("default schedule = %q, want %q", cfg.Schedule.Cron, DefaultSchedule)
	}
}

// A container that is recreated would write to the library by itself if this
// defaulted to true, which is the opposite of what the tool does everywhere else.
func TestRunOnStartDefaultsToOff(t *testing.T) {
	if Default().Schedule.RunOnStart {
		t.Error("run_on_start defaults to on")
	}
}

func TestScheduleFromTheEnvironment(t *testing.T) {
	t.Setenv("PLEX_SYNC_SCHEDULE", "0 5 * * *")
	t.Setenv("PLEX_SYNC_RUN_ON_START", "true")

	cfg := Default()
	cfg.applyEnv()

	if cfg.Schedule.Cron != "0 5 * * *" {
		t.Errorf("cron = %q, want the environment value", cfg.Schedule.Cron)
	}
	if !cfg.Schedule.RunOnStart {
		t.Error("run_on_start was not taken from the environment")
	}
}

func TestScheduleFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[schedule]\ncron = \"15 3 * * 1\"\nrun_on_start = true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Schedule.Cron != "15 3 * * 1" {
		t.Errorf("cron = %q, want the file value", cfg.Schedule.Cron)
	}
	if !cfg.Schedule.RunOnStart {
		t.Error("run_on_start was not taken from the file")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the file's schedule does not validate: %v", err)
	}
}

// The example config is what `config init` writes, so it has to load and
// validate or every new user starts with a broken file.
func TestExampleConfigSchedulesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(Example()), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the example config does not validate: %v", err)
	}
	if cfg.Schedule.Cron == "" {
		t.Error("the example config does not mention the schedule")
	}
}
