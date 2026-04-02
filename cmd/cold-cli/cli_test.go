package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI builds the cold-cli binary into a temp dir and returns its path.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cold-cli")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(".") // cmd/cold-cli
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building cold-cli: %v\n%s", err, out)
	}
	return bin
}

// setupTestEnv creates a temp data dir and returns the bin path and env.
func setupTestEnv(t *testing.T) (bin string, env []string, dataDir string) {
	t.Helper()
	bin = buildCLI(t)
	dataDir = t.TempDir()
	env = append(os.Environ(), "COLD_CLI_DATA_DIR="+dataDir)
	return bin, env, dataDir
}

// runCLI runs cold-cli with the given args and env, returns stdout and exit code.
func runCLI(t *testing.T, bin string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("running cold-cli %v: %v", args, err)
		}
	}
	return string(out), exitCode
}

// --- init tests ---

func TestCLI_Init(t *testing.T) {
	bin, env, dataDir := setupTestEnv(t)

	out, code := runCLI(t, bin, env, "init")
	if code != 0 {
		t.Fatalf("init failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Initialized cold-cli") {
		t.Errorf("unexpected init output: %s", out)
	}

	// Verify files created
	if _, err := os.Stat(filepath.Join(dataDir, "data.db")); err != nil {
		t.Error("data.db not created")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.yml")); err != nil {
		t.Error("config.yml not created")
	}
}

func TestCLI_Init_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)

	out, code := runCLI(t, bin, env, "init", "--json")
	if code != 0 {
		t.Fatalf("init --json failed (exit %d): %s", code, out)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, out)
	}
	if _, ok := result["data_dir"]; !ok {
		t.Error("JSON missing data_dir key")
	}
	if _, ok := result["gws_ok"]; !ok {
		t.Error("JSON missing gws_ok key")
	}
}

func TestCLI_Init_Idempotent(t *testing.T) {
	bin, env, _ := setupTestEnv(t)

	runCLI(t, bin, env, "init")
	out, code := runCLI(t, bin, env, "init")
	if code != 0 {
		t.Fatalf("second init failed (exit %d): %s", code, out)
	}
}

// --- account tests ---

func TestCLI_AccountAdd(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "test@example.com")
	if code != 0 {
		t.Fatalf("account add failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Added account test@example.com") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestCLI_AccountAdd_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "test@example.com", "--json")
	if code != 0 {
		t.Fatalf("account add --json failed (exit %d): %s", code, out)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result["email"] != "test@example.com" {
		t.Errorf("expected email test@example.com, got %v", result["email"])
	}
	if result["status"] != "active" {
		t.Errorf("expected status active, got %v", result["status"])
	}
}

func TestCLI_AccountAdd_InvalidEmail(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	_, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "not-an-email")
	if code == 0 {
		t.Error("expected non-zero exit for invalid email")
	}
}

func TestCLI_AccountAdd_Duplicate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "dup@example.com")

	out, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "dup@example.com")
	if code == 0 {
		t.Error("expected non-zero exit for duplicate")
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("expected 'already exists' error, got: %s", out)
	}
}

func TestCLI_AccountAdd_DailyLimit(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "test@example.com", "--daily-limit", "25", "--json")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}

	var result map[string]any
	json.Unmarshal([]byte(out), &result)
	if result["daily_limit"] != float64(25) {
		t.Errorf("expected daily_limit 25, got %v", result["daily_limit"])
	}
}

func TestCLI_AccountAdd_MissingArg(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	_, code := runCLI(t, bin, env, "account", "add")
	if code == 0 {
		t.Error("expected non-zero exit for missing email arg")
	}
}

func TestCLI_AccountList_Empty(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "account", "list")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "No accounts") {
		t.Errorf("expected empty message, got: %s", out)
	}
}

func TestCLI_AccountList(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "a@x.com")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "b@x.com")

	out, code := runCLI(t, bin, env, "account", "list")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "a@x.com") || !strings.Contains(out, "b@x.com") {
		t.Errorf("expected both accounts listed: %s", out)
	}
}

func TestCLI_AccountList_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "a@x.com")

	out, code := runCLI(t, bin, env, "account", "list", "--json")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}

	var result []map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 account, got %d", len(result))
	}
}

// --- campaign tests ---

func setupCampaignTestFiles(t *testing.T) (seqFile, leadsFile string) {
	t.Helper()
	dir := t.TempDir()

	seqFile = filepath.Join(dir, "seq.yml")
	os.WriteFile(seqFile, []byte(`
name: Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
  - step: 2
    delay: 3
    body: "Following up..."
`), 0644)

	leadsFile = filepath.Join(dir, "leads.csv")
	os.WriteFile(leadsFile, []byte("email,first_name,company\njohn@acme.com,John,Acme\njane@foo.com,Jane,Foo\n"), 0644)

	return seqFile, leadsFile
}

func TestCLI_CampaignCreate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "create",
		"--name", "test-camp",
		"--sequence", seqFile,
		"--leads", leadsFile,
		"--accounts", "sender@x.com")
	if code != 0 {
		t.Fatalf("campaign create failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Created campaign") {
		t.Errorf("unexpected output: %s", out)
	}
	if !strings.Contains(out, "leads:    2") {
		t.Errorf("expected 2 leads: %s", out)
	}
	if !strings.Contains(out, "sends:    4") {
		t.Errorf("expected 4 sends (2 leads * 2 steps): %s", out)
	}
}

func TestCLI_CampaignCreate_SendDaysOverride(t *testing.T) {
	bin, env, dataDir := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	os.WriteFile(filepath.Join(dataDir, "config.yml"), []byte(`default_timezone: UTC
default_daily_limit: 50
min_gap_seconds: 90
max_gap_seconds: 140
send_window_start: "09:00"
send_window_end: "17:00"
send_days: "1,2,3,4,5"
`), 0644)

	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	dir := t.TempDir()
	seqFile := filepath.Join(dir, "seq.yml")
	os.WriteFile(seqFile, []byte(`
steps:
  - step: 1
    delay: 0
    subject: "Hi"
    body: "Hello"
`), 0644)
	leadsFile := filepath.Join(dir, "leads.csv")
	os.WriteFile(leadsFile, []byte("email\njohn@acme.com\n"), 0644)

	out, code := runCLI(t, bin, env, "campaign", "create",
		"--name", "weekend-camp",
		"--sequence", seqFile,
		"--leads", leadsFile,
		"--accounts", "sender@x.com",
		"--start-date", "2026-06-13",
		"--send-days", "0,1,2,3,4,5,6")
	if code != 0 {
		t.Fatalf("campaign create with send-days override failed (exit %d): %s", code, out)
	}

	out, code = runCLI(t, bin, env, "campaign", "preview", "weekend-camp")
	if code != 0 {
		t.Fatalf("campaign preview failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "2026-06-13") {
		t.Errorf("expected preview to keep the Saturday start date, got: %s", out)
	}
}

func TestCLI_CampaignCreate_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "create",
		"--name", "test-camp",
		"--sequence", seqFile,
		"--leads", leadsFile,
		"--accounts", "sender@x.com",
		"--json")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}

	var result map[string]any
	json.Unmarshal([]byte(out), &result)
	if result["status"] != "draft" {
		t.Errorf("expected status draft, got %v", result["status"])
	}
	if result["leads"] != float64(2) {
		t.Errorf("expected 2 leads, got %v", result["leads"])
	}
}

func TestCLI_CampaignCreate_MissingFlags(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	_, code := runCLI(t, bin, env, "campaign", "create", "--name", "test")
	if code == 0 {
		t.Error("expected error for missing required flags")
	}
}

func TestCLI_CampaignCreate_DuplicateName(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "dup", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "create", "--name", "dup", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	if code == 0 {
		t.Error("expected error for duplicate campaign name")
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("expected 'already exists' error: %s", out)
	}
}

func TestCLI_CampaignCreate_BadAccount(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "campaign", "create", "--name", "test", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "nonexistent@x.com")
	if code == 0 {
		t.Error("expected error for bad account")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error: %s", out)
	}
}

func TestCLI_CampaignCreate_MissingTemplateField(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	dir := t.TempDir()
	seqFile := filepath.Join(dir, "seq.yml")
	os.WriteFile(seqFile, []byte(`
steps:
  - step: 1
    subject: "Hi {{first_name}} at {{title}}"
    body: "Hello {{first_name}}"
`), 0644)
	leadsFile := filepath.Join(dir, "leads.csv")
	os.WriteFile(leadsFile, []byte("email,first_name\njohn@acme.com,John\n"), 0644)

	out, code := runCLI(t, bin, env, "campaign", "create", "--name", "test", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	if code == 0 {
		t.Error("expected error for missing template field 'title'")
	}
	if !strings.Contains(out, "title") {
		t.Errorf("expected error mentioning 'title': %s", out)
	}
}

func TestCLI_CampaignPreview(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "preview", "test-camp")
	if code != 0 {
		t.Fatalf("preview failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "john@acme.com") {
		t.Errorf("expected john in preview: %s", out)
	}
	if !strings.Contains(out, "jane@foo.com") {
		t.Errorf("expected jane in preview: %s", out)
	}
	if !strings.Contains(out, "4 sends") {
		t.Errorf("expected '4 sends' in preview: %s", out)
	}
}

func TestCLI_CampaignPreview_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "campaign", "preview", "nonexistent")
	if code == 0 {
		t.Error("expected error for nonexistent campaign")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error: %s", out)
	}
}

func TestCLI_CampaignActivate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "activate", "test-camp")
	if code != 0 {
		t.Fatalf("activate failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "now active") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestCLI_CampaignActivate_WrongState(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	runCLI(t, bin, env, "campaign", "activate", "test-camp")

	// Try to activate again (already active)
	out, code := runCLI(t, bin, env, "campaign", "activate", "test-camp")
	if code == 0 {
		t.Error("expected error for activating already active campaign")
	}
	if !strings.Contains(out, "cannot activate") {
		t.Errorf("expected state transition error: %s", out)
	}
}

func TestCLI_CampaignPauseResume(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	runCLI(t, bin, env, "campaign", "activate", "test-camp")

	// Pause
	out, code := runCLI(t, bin, env, "campaign", "pause", "test-camp")
	if code != 0 {
		t.Fatalf("pause failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "now paused") {
		t.Errorf("unexpected output: %s", out)
	}

	// Resume
	out, code = runCLI(t, bin, env, "campaign", "resume", "test-camp")
	if code != 0 {
		t.Fatalf("resume failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "now active") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestCLI_CampaignStatus(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "status", "test-camp")
	if code != 0 {
		t.Fatalf("status failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "draft") {
		t.Errorf("expected draft status: %s", out)
	}
	if !strings.Contains(out, "leads:       2") {
		t.Errorf("expected 2 leads: %s", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("expected pending sends: %s", out)
	}
}

func TestCLI_CampaignStatus_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "status", "test-camp", "--json")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result["leads"] != float64(2) {
		t.Errorf("expected 2 leads, got %v", result["leads"])
	}
}

// --- lead tests ---

func TestCLI_LeadBlacklist(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "lead", "blacklist", "john@acme.com")
	if code != 0 {
		t.Fatalf("blacklist failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Blacklisted john@acme.com") {
		t.Errorf("unexpected output: %s", out)
	}
	if !strings.Contains(out, "sends cancelled") {
		t.Errorf("expected cancelled sends: %s", out)
	}
}

func TestCLI_LeadBlacklist_Domain(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "lead", "blacklist", "acme.com", "--json")
	if code != 0 {
		t.Fatalf("blacklist domain failed (exit %d): %s", code, out)
	}

	var result map[string]any
	json.Unmarshal([]byte(out), &result)
	if result["is_domain"] != true {
		t.Errorf("expected is_domain=true, got %v", result["is_domain"])
	}
	if result["blacklisted_leads"] != float64(1) {
		t.Errorf("expected 1 blacklisted lead (john@acme.com), got %v", result["blacklisted_leads"])
	}
}

func TestCLI_LeadBlacklist_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "lead", "blacklist", "nobody@x.com")
	if code == 0 {
		t.Error("expected error for blacklisting nonexistent lead")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error: %s", out)
	}
}

func TestCLI_LeadPause(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "lead", "pause", "john@acme.com")
	if code != 0 {
		t.Fatalf("pause failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Paused john@acme.com") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestCLI_LeadPause_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "lead", "pause", "nobody@x.com")
	if code == 0 {
		t.Error("expected error for pausing nonexistent lead")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error: %s", out)
	}
}

// --- tick tests ---

func TestCLI_Tick_DryRun(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "tick", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("tick dry-run failed (exit %d): %s", code, out)
	}

	var result map[string]any
	json.Unmarshal([]byte(out), &result)
	if result["dry_run"] != true {
		t.Errorf("expected dry_run=true, got %v", result["dry_run"])
	}
}

// --- stats tests ---

func TestCLI_Stats_Empty(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "stats")
	if code != 0 {
		t.Fatalf("stats failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "No campaigns") {
		t.Errorf("expected empty state message: %s", out)
	}
}

func TestCLI_Stats_WithCampaign(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "stats")
	if code != 0 {
		t.Fatalf("stats failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "test-camp") {
		t.Errorf("expected campaign name in stats: %s", out)
	}
}

func TestCLI_Stats_PerCampaign(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "stats", "test-camp")
	if code != 0 {
		t.Fatalf("stats failed (exit %d): %s", code, out)
	}
	// No events yet, so "no events" message
	if !strings.Contains(out, "no events") {
		t.Errorf("expected 'no events' message: %s", out)
	}
}

func TestCLI_Stats_PerLead(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "test-camp", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "stats", "test-camp", "--leads")
	if code != 0 {
		t.Fatalf("stats --leads failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "john@acme.com") {
		t.Errorf("expected john in lead stats: %s", out)
	}
	if !strings.Contains(out, "jane@foo.com") {
		t.Errorf("expected jane in lead stats: %s", out)
	}
}

func TestCLI_Stats_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "stats", "nonexistent")
	if code == 0 {
		t.Error("expected error for nonexistent campaign")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' error: %s", out)
	}
}

// --- campaign list/delete/update tests ---

func TestCLI_CampaignList_Empty(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "campaign", "list")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "No campaigns") {
		t.Errorf("expected empty message: %s", out)
	}
}

func TestCLI_CampaignList(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "camp-a", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "camp-b", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "list")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "camp-a") || !strings.Contains(out, "camp-b") {
		t.Errorf("expected both campaigns: %s", out)
	}
}

func TestCLI_CampaignList_JSON(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "camp-a", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "list", "--json")
	if code != 0 {
		t.Fatalf("failed (exit %d): %s", code, out)
	}
	var result []map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 campaign, got %d", len(result))
	}
}

func TestCLI_CampaignDelete(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "to-delete", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "delete", "to-delete")
	if code != 0 {
		t.Fatalf("delete failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Deleted") {
		t.Errorf("expected 'Deleted' message: %s", out)
	}

	// Verify it's gone
	out, code = runCLI(t, bin, env, "campaign", "status", "to-delete")
	if code == 0 {
		t.Error("expected error for deleted campaign")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found': %s", out)
	}
}

func TestCLI_CampaignDelete_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "campaign", "delete", "nonexistent")
	if code == 0 {
		t.Error("expected error for nonexistent campaign")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found': %s", out)
	}
}

func TestCLI_CampaignDelete_Recreate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	// Create, delete, recreate with same name
	runCLI(t, bin, env, "campaign", "create", "--name", "reuse", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	runCLI(t, bin, env, "campaign", "delete", "reuse")
	out, code := runCLI(t, bin, env, "campaign", "create", "--name", "reuse", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")
	if code != 0 {
		t.Fatalf("recreate failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Created campaign") {
		t.Errorf("expected 'Created': %s", out)
	}
}

func TestCLI_CampaignUpdate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "updatable", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "update", "updatable", "--send-days", "2,3,4")
	if code != 0 {
		t.Fatalf("update failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Updated") {
		t.Errorf("expected 'Updated': %s", out)
	}

	// Verify via status
	out, code = runCLI(t, bin, env, "campaign", "status", "updatable", "--json")
	if code != 0 {
		t.Fatalf("status failed (exit %d): %s", code, out)
	}
}

func TestCLI_CampaignUpdate_NoFlags(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "no-update", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "update", "no-update")
	if code == 0 {
		t.Error("expected error when no flags provided")
	}
	if !strings.Contains(out, "no settings") {
		t.Errorf("expected guidance message: %s", out)
	}
}

func TestCLI_CampaignUpdate_NotFound(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	out, code := runCLI(t, bin, env, "campaign", "update", "nonexistent", "--send-days", "1,2,3")
	if code == 0 {
		t.Error("expected error for nonexistent campaign")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found': %s", out)
	}
}

func TestCLI_CampaignPreview_ShowsSchedule(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "preview-note", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "preview", "preview-note")
	if code != 0 {
		t.Fatalf("preview failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "SEND AT") || !strings.Contains(out, "STEP") {
		t.Errorf("expected schedule table in preview: %s", out)
	}
}

func TestCLI_CampaignHelp_NewCommands(t *testing.T) {
	bin, env, _ := setupTestEnv(t)

	out, code := runCLI(t, bin, env, "campaign", "--help")
	if code != 0 {
		t.Fatalf("help failed (exit %d): %s", code, out)
	}
	for _, sub := range []string{"list", "delete", "update"} {
		if !strings.Contains(out, sub) {
			t.Errorf("campaign help missing new subcommand %q: %s", sub, out)
		}
	}
}

// --- error handling tests ---

func TestCLI_NotInitialized(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	// Don't run init

	out, code := runCLI(t, bin, env, "account", "add", "--skip-auth", "test@x.com")
	if code == 0 {
		t.Error("expected error when not initialized")
	}
	if !strings.Contains(out, "cold-cli init") {
		t.Errorf("expected guidance to run init: %s", out)
	}
}

func TestCLI_Help(t *testing.T) {
	bin, env, _ := setupTestEnv(t)

	out, code := runCLI(t, bin, env, "--help")
	if code != 0 {
		t.Fatalf("help failed (exit %d): %s", code, out)
	}
	for _, cmd := range []string{"init", "account", "campaign", "tick", "stats", "lead"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("help missing command %q: %s", cmd, out)
		}
	}
}

func TestCLI_CampaignHelp(t *testing.T) {
	bin, env, _ := setupTestEnv(t)

	out, code := runCLI(t, bin, env, "campaign", "--help")
	if code != 0 {
		t.Fatalf("help failed (exit %d): %s", code, out)
	}
	for _, sub := range []string{"create", "preview", "activate", "pause", "resume", "status"} {
		if !strings.Contains(out, sub) {
			t.Errorf("campaign help missing subcommand %q: %s", sub, out)
		}
	}
}

// --- campaign init tests ---

func TestCLI_CampaignInit(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")

	dir := t.TempDir()
	out, code := runCLI(t, bin, env, "campaign", "init", dir)
	if code != 0 {
		t.Fatalf("campaign init failed (exit %d): %s", code, out)
	}

	// Verify sequence.yml was created
	if _, err := os.Stat(filepath.Join(dir, "sequence.yml")); err != nil {
		t.Error("sequence.yml not created")
	}
	// Verify leads.csv was created
	if _, err := os.Stat(filepath.Join(dir, "leads.csv")); err != nil {
		t.Error("leads.csv not created")
	}
}

// --- account update tests ---

func TestCLI_AccountUpdate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	out, code := runCLI(t, bin, env, "account", "update", "sender@x.com", "--daily-limit", "25")
	if code != 0 {
		t.Fatalf("account update failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "Updated") {
		t.Errorf("expected 'Updated' in output, got: %s", out)
	}
}

// --- campaign preview/status by ID tests ---

func TestCLI_CampaignPreviewByID(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "by-id-preview", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	// Preview by numeric ID instead of name
	out, code := runCLI(t, bin, env, "campaign", "preview", "1")
	if code != 0 {
		t.Fatalf("preview by ID failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "john@acme.com") {
		t.Errorf("expected john in preview: %s", out)
	}
	if !strings.Contains(out, "jane@foo.com") {
		t.Errorf("expected jane in preview: %s", out)
	}
}

func TestCLI_CampaignStatusByID(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "by-id-status", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	// Status by numeric ID instead of name
	out, code := runCLI(t, bin, env, "campaign", "status", "1")
	if code != 0 {
		t.Fatalf("status by ID failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "draft") {
		t.Errorf("expected draft status: %s", out)
	}
	if !strings.Contains(out, "by-id-status") {
		t.Errorf("expected campaign name in output: %s", out)
	}
}

// --- campaign start-date tests ---

func TestCLI_CampaignStartDate(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "create",
		"--name", "start-date-test",
		"--sequence", seqFile,
		"--leads", leadsFile,
		"--accounts", "sender@x.com",
		"--start-date", "2026-04-01")
	if code != 0 {
		t.Fatalf("campaign create with start-date failed (exit %d): %s", code, out)
	}

	// Preview to see scheduled send dates
	out, code = runCLI(t, bin, env, "campaign", "preview", "start-date-test")
	if code != 0 {
		t.Fatalf("preview failed (exit %d): %s", code, out)
	}
	// Send dates should be in April 2026, not March
	if !strings.Contains(out, "2026-04") {
		t.Errorf("expected send dates in April 2026, got: %s", out)
	}
	if strings.Contains(out, "2026-03") {
		t.Errorf("expected no send dates in March 2026, but found them: %s", out)
	}
}

// --- campaign preview --render tests ---

func TestCLI_CampaignPreviewRender(t *testing.T) {
	bin, env, _ := setupTestEnv(t)
	seqFile, leadsFile := setupCampaignTestFiles(t)
	runCLI(t, bin, env, "init")
	runCLI(t, bin, env, "account", "add", "--skip-auth", "sender@x.com")
	runCLI(t, bin, env, "campaign", "create", "--name", "render-test", "--sequence", seqFile, "--leads", leadsFile, "--accounts", "sender@x.com")

	out, code := runCLI(t, bin, env, "campaign", "preview", "render-test", "--render")
	if code != 0 {
		t.Fatalf("preview --render failed (exit %d): %s", code, out)
	}

	// Should contain rendered subject with actual name, not placeholder
	if !strings.Contains(out, "Subject:") {
		t.Errorf("expected 'Subject:' in rendered preview: %s", out)
	}
	// The first lead (alphabetically by send_at) should have placeholders filled in.
	// setupCampaignTestFiles creates leads john@acme.com (John) and jane@foo.com (Jane).
	// Check that at least one rendered name appears and no raw placeholders remain.
	hasRenderedName := strings.Contains(out, "John") || strings.Contains(out, "Jane")
	if !hasRenderedName {
		t.Errorf("expected rendered first_name (John or Jane) in output: %s", out)
	}
	hasRenderedCompany := strings.Contains(out, "Acme") || strings.Contains(out, "Foo")
	if !hasRenderedCompany {
		t.Errorf("expected rendered company (Acme or Foo) in output: %s", out)
	}
	if strings.Contains(out, "{{first_name}}") {
		t.Errorf("output still contains raw {{first_name}} placeholder: %s", out)
	}
	if strings.Contains(out, "{{company}}") {
		t.Errorf("output still contains raw {{company}} placeholder: %s", out)
	}
}
