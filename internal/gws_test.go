package internal

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCheckGWSInstalled(t *testing.T) {
	err := CheckGWSInstalled()
	_, lookErr := exec.LookPath("gws")
	if lookErr != nil {
		if err == nil {
			t.Error("expected error when gws not on PATH")
		}
	} else {
		if err != nil {
			t.Errorf("unexpected error when gws is on PATH: %v", err)
		}
	}
}

func TestGWSConfigDirForAccount(t *testing.T) {
	// Override data dir to temp
	dir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", dir)

	configDir := GWSConfigDirForAccount("test@example.com")

	// Should create the directory
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("config dir not created: %v", err)
	}

	// Should contain sanitized email in path
	if !filepath.IsAbs(configDir) {
		t.Errorf("expected absolute path, got %q", configDir)
	}

	// Path should not contain @ or dots (sanitized)
	base := filepath.Base(configDir)
	if base != "test-at-example-com" {
		t.Errorf("expected sanitized dir name 'test-at-example-com', got %q", base)
	}

	// Calling again should return same path (idempotent)
	configDir2 := GWSConfigDirForAccount("test@example.com")
	if configDir != configDir2 {
		t.Errorf("expected same path, got %q vs %q", configDir, configDir2)
	}
}

func TestGWSConfigDirForAccount_CopiesClientSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", dir)

	// Create a fake ~/.config/gws/client_secret.json
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	gwsConfigDir := filepath.Join(fakeHome, ".config", "gws")
	os.MkdirAll(gwsConfigDir, 0755)
	secretContent := `{"client_id": "test", "client_secret": "test"}`
	os.WriteFile(filepath.Join(gwsConfigDir, "client_secret.json"), []byte(secretContent), 0600)

	configDir := GWSConfigDirForAccount("copy@example.com")

	// Should have copied client_secret.json
	destSecret := filepath.Join(configDir, "client_secret.json")
	data, err := os.ReadFile(destSecret)
	if err != nil {
		t.Fatalf("client_secret.json not copied: %v", err)
	}
	if string(data) != secretContent {
		t.Errorf("client_secret.json content mismatch: got %q", string(data))
	}
}

func TestGWSConfigDirForAccount_NoSourceSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", dir)

	// Set HOME to empty dir (no .config/gws/client_secret.json)
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Should not panic or error — just skip the copy
	configDir := GWSConfigDirForAccount("nosecret@example.com")

	// Dir should exist but no client_secret.json
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("config dir not created: %v", err)
	}
	destSecret := filepath.Join(configDir, "client_secret.json")
	if _, err := os.Stat(destSecret); !os.IsNotExist(err) {
		t.Error("client_secret.json should not exist when source is missing")
	}
}

func TestGWSErrorResponseDetection(t *testing.T) {
	// Test the error response struct parsing
	tests := []struct {
		name    string
		json    string
		isError bool
		code    int
	}{
		{
			"API error",
			`{"error": {"code": 403, "message": "Delegation denied"}}`,
			true, 403,
		},
		{
			"success response",
			`{"id": "msg-123", "threadId": "thread-456"}`,
			false, 0,
		},
		{
			"empty error",
			`{"error": null}`,
			false, 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp gwsErrorResponse
			if err := json.Unmarshal([]byte(tt.json), &resp); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			hasError := resp.Error != nil
			if hasError != tt.isError {
				t.Errorf("expected isError=%v, got %v", tt.isError, hasError)
			}
			if hasError && resp.Error.Code != tt.code {
				t.Errorf("expected code %d, got %d", tt.code, resp.Error.Code)
			}
		})
	}
}

func TestGWSCLI_SetConfigDir(t *testing.T) {
	cli := NewGWSCLI()

	cli.SetConfigDir("a@x.com", "/path/to/a")
	cli.SetConfigDir("b@x.com", "/path/to/b")

	if cli.ConfigDirs["a@x.com"] != "/path/to/a" {
		t.Errorf("expected /path/to/a, got %s", cli.ConfigDirs["a@x.com"])
	}
	if cli.ConfigDirs["b@x.com"] != "/path/to/b" {
		t.Errorf("expected /path/to/b, got %s", cli.ConfigDirs["b@x.com"])
	}
}
