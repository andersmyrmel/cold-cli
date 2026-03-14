package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	if err := WriteDefaultConfig(path); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	// Verify file was written
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// Read it back — patch ConfigPath by reading directly
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}

	if len(data) == 0 {
		t.Error("config file is empty")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig

	if cfg.DefaultTimezone != "America/New_York" {
		t.Errorf("unexpected timezone: %s", cfg.DefaultTimezone)
	}
	if cfg.DefaultDailyLimit != 50 {
		t.Errorf("unexpected daily limit: %d", cfg.DefaultDailyLimit)
	}
	if cfg.MinGapSeconds != 90 {
		t.Errorf("unexpected min gap: %d", cfg.MinGapSeconds)
	}
	if cfg.MaxGapSeconds != 140 {
		t.Errorf("unexpected max gap: %d", cfg.MaxGapSeconds)
	}
	if cfg.SendWindowStart != "09:00" {
		t.Errorf("unexpected send window start: %s", cfg.SendWindowStart)
	}
	if cfg.SendWindowEnd != "17:00" {
		t.Errorf("unexpected send window end: %s", cfg.SendWindowEnd)
	}
	if cfg.SendDays != "1,2,3,4,5" {
		t.Errorf("unexpected send days: %s", cfg.SendDays)
	}
}
