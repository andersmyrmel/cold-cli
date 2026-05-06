package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFileSetsVariables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	content := `
# comments and blank lines are ignored
MAIL_PASSWORD=plain-secret
export API_TOKEN='single quoted secret'
DOUBLE_QUOTED="double quoted\nsecret"
EMPTY_VALUE=
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAIL_PASSWORD", "old-value")
	t.Setenv("API_TOKEN", "")
	t.Setenv("DOUBLE_QUOTED", "")
	t.Setenv("EMPTY_VALUE", "old-value")

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile error: %v", err)
	}

	if got := os.Getenv("MAIL_PASSWORD"); got != "plain-secret" {
		t.Fatalf("MAIL_PASSWORD = %q", got)
	}
	if got := os.Getenv("API_TOKEN"); got != "single quoted secret" {
		t.Fatalf("API_TOKEN = %q", got)
	}
	if got := os.Getenv("DOUBLE_QUOTED"); got != "double quoted\nsecret" {
		t.Fatalf("DOUBLE_QUOTED = %q", got)
	}
	if got := os.Getenv("EMPTY_VALUE"); got != "" {
		t.Fatalf("EMPTY_VALUE = %q", got)
	}
}

func TestLoadEnvFileRejectsInvalidLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte("MAIL_PASSWORD\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := LoadEnvFile(path); err == nil {
		t.Fatal("expected invalid line error")
	}
}

func TestLoadEnvFileRejectsInvalidKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte("1PASSWORD=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := LoadEnvFile(path); err == nil {
		t.Fatal("expected invalid key error")
	}
}
