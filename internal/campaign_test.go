package internal

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestResolveCampaignName_ByName(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('my-campaign', 'seq.yml')")

	name, err := ResolveCampaignName(db, "my-campaign")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my-campaign" {
		t.Errorf("expected 'my-campaign', got %q", name)
	}
}

func TestResolveCampaignName_ByID(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('my-campaign', 'seq.yml')")

	name, err := ResolveCampaignName(db, "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my-campaign" {
		t.Errorf("expected 'my-campaign', got %q", name)
	}
}

func TestResolveCampaignName_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := ResolveCampaignName(db, "nope")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}

	_, err = ResolveCampaignName(db, "999")
	if err == nil {
		t.Error("expected error for non-existent campaign ID")
	}
}

func TestGetCampaignRenderedPreview(t *testing.T) {
	db := testDB(t)

	seqYAML := `name: Test
defaults:
  from_name: "Tester"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
  - step: 2
    delay: 3
    subject: ""
    body: "Following up, {{first_name}}"
`

	// Insert account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Insert campaign with sequence_content
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone)
		VALUES ('render-test', 'draft', 'seq.yml', ?, '09:00', '17:00', '1,2,3,4,5', 'America/New_York')`,
		seqYAML)

	// Insert lead
	db.Exec(`INSERT INTO leads (email, first_name, last_name, company, domain)
		VALUES ('alice@acme.com', 'Alice', 'Smith', 'Acme Corp', 'acme.com')`)

	// Insert campaign_leads link
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	// Insert scheduled_sends for both steps
	sendAt := time.Now().UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at)
		VALUES (1, 1, 1, 1, 0, ?)`, sendAt)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at)
		VALUES (1, 1, 1, 2, 0, ?)`, sendAt)

	rendered, err := GetCampaignRenderedPreview(db, "render-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rendered) != 2 {
		t.Fatalf("expected 2 rendered emails, got %d", len(rendered))
	}

	// Step 1: subject and body should have placeholders filled in
	if rendered[0].StepNumber != 1 {
		t.Errorf("expected step 1, got %d", rendered[0].StepNumber)
	}
	if rendered[0].Subject != "Hi Alice" {
		t.Errorf("expected rendered subject 'Hi Alice', got %q", rendered[0].Subject)
	}
	if !strings.Contains(rendered[0].Body, "Hello Alice at Acme Corp") {
		t.Errorf("expected rendered body to contain 'Hello Alice at Acme Corp', got %q", rendered[0].Body)
	}
	if rendered[0].LeadEmail != "alice@acme.com" {
		t.Errorf("expected lead email 'alice@acme.com', got %q", rendered[0].LeadEmail)
	}
	if rendered[0].AccountEmail != "sender@x.com" {
		t.Errorf("expected account email 'sender@x.com', got %q", rendered[0].AccountEmail)
	}

	// Step 2: body should have placeholder filled in
	if rendered[1].StepNumber != 2 {
		t.Errorf("expected step 2, got %d", rendered[1].StepNumber)
	}
	if !strings.Contains(rendered[1].Body, "Following up, Alice") {
		t.Errorf("expected rendered body to contain 'Following up, Alice', got %q", rendered[1].Body)
	}

	// Verify no raw placeholders remain
	for i, r := range rendered {
		if strings.Contains(r.Subject, "{{") || strings.Contains(r.Body, "{{") {
			t.Errorf("rendered email %d still contains raw placeholders: subject=%q body=%q", i, r.Subject, r.Body)
		}
	}
}

func TestGetCampaignRenderedPreview_SpecificLead(t *testing.T) {
	db := testDB(t)

	seqYAML := `name: Test
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}}"
`
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone)
		VALUES ('lead-test', 'draft', 'seq.yml', ?, '09:00', '17:00', '1,2,3,4,5', 'UTC')`, seqYAML)

	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('alice@acme.com', 'Alice', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('bob@acme.com', 'Bob', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 2, 'active')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at) VALUES (1, 1, 1, 1, 0, '2025-01-01')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at) VALUES (1, 2, 1, 1, 0, '2025-01-01')")

	// Request render for Bob specifically
	rendered, err := GetCampaignRenderedPreview(db, "lead-test", "bob@acme.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rendered) != 1 {
		t.Fatalf("expected 1 rendered email, got %d", len(rendered))
	}
	if rendered[0].LeadEmail != "bob@acme.com" {
		t.Errorf("expected bob@acme.com, got %q", rendered[0].LeadEmail)
	}
	if rendered[0].Subject != "Hi Bob" {
		t.Errorf("expected 'Hi Bob', got %q", rendered[0].Subject)
	}
}

func TestGetCampaignRenderedPreview_LeadNotInCampaign(t *testing.T) {
	db := testDB(t)

	seqYAML := `name: Test
steps:
  - step: 1
    delay: 0
    subject: "Hi"
    body: "Hello"
`
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone)
		VALUES ('lead-test2', 'draft', 'seq.yml', ?, '09:00', '17:00', '1,2,3,4,5', 'UTC')`, seqYAML)

	_, err := GetCampaignRenderedPreview(db, "lead-test2", "nobody@acme.com")
	if err == nil {
		t.Error("expected error for lead not in campaign")
	}
}

func TestGetCampaignRenderedPreview_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := GetCampaignRenderedPreview(db, "nonexistent", "")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestGetDailyLimitWarnings_NoWarnings(t *testing.T) {
	db := testDB(t)

	warnings, err := GetDailyLimitWarnings(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings, got %d", len(warnings))
	}
}

func TestUpdateCampaign_Sequence(t *testing.T) {
	db := testDB(t)

	origYAML := `name: Original
defaults:
  from_name: Tester
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
`
	// Set up campaign with leads
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content)
		VALUES ('seq-update-test', 'draft', 'old.yml', ?)`, origYAML)
	db.Exec(`INSERT INTO leads (email, first_name, company, domain)
		VALUES ('alice@acme.com', 'Alice', 'Acme', 'acme.com')`)
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	// Write new sequence to temp file
	newYAML := `name: Updated
defaults:
  from_name: Tester
steps:
  - step: 1
    delay: 0
    subject: "Hey {{first_name}}"
    body: "New body for {{first_name}} at {{company}}"
  - step: 2
    delay: 3
    subject: ""
    body: "Follow up {{first_name}}"
`
	tmpFile := t.TempDir() + "/new-seq.yml"
	if err := os.WriteFile(tmpFile, []byte(newYAML), 0644); err != nil {
		t.Fatal(err)
	}

	err := UpdateCampaign(db, "seq-update-test", UpdateCampaignOpts{
		SequenceFile: &tmpFile,
	})
	if err != nil {
		t.Fatalf("UpdateCampaign with sequence: %v", err)
	}

	// Verify sequence_content was updated
	var content string
	db.QueryRow("SELECT sequence_content FROM campaigns WHERE name = 'seq-update-test'").Scan(&content)
	if !strings.Contains(content, "New body") {
		t.Errorf("expected updated sequence content, got: %s", content)
	}
	if !strings.Contains(content, "Follow up") {
		t.Errorf("expected step 2 in updated content, got: %s", content)
	}
}

func TestUpdateCampaign_Sequence_BadPlaceholder(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content)
		VALUES ('seq-bad-test', 'draft', 'old.yml', 'name: X')`)
	db.Exec(`INSERT INTO leads (email, first_name, company, domain)
		VALUES ('alice@acme.com', 'Alice', 'Acme', 'acme.com')`)
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	// New sequence uses {{title}} which no lead has
	badYAML := `name: Bad
defaults:
  from_name: Tester
steps:
  - step: 1
    delay: 0
    subject: "Hi {{title}}"
    body: "Hello {{title}}"
`
	tmpFile := t.TempDir() + "/bad-seq.yml"
	os.WriteFile(tmpFile, []byte(badYAML), 0644)

	err := UpdateCampaign(db, "seq-bad-test", UpdateCampaignOpts{
		SequenceFile: &tmpFile,
	})
	if err == nil {
		t.Fatal("expected error for bad placeholder, got nil")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("expected error to mention 'title', got: %v", err)
	}
}

func TestRetryCampaign_AllFailed(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file) VALUES ('retry-test', 'active', 'seq.yml')`)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('a@b.com', 'A', 'B', 'b.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('c@d.com', 'C', 'D', 'd.com')")

	pastTime := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 1, ?, 'failed')`, pastTime)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 2, 1, 1, ?, 'failed')`, pastTime)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, ?, 'pending')`, pastTime)

	result, err := RetryCampaign(db, "retry-test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Retried != 2 {
		t.Errorf("expected 2 retried, got %d", result.Retried)
	}

	// Verify statuses
	var count int
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE campaign_id = 1 AND status = 'pending'").Scan(&count)
	if count != 3 {
		t.Errorf("expected 3 pending (2 retried + 1 original), got %d", count)
	}
}

func TestRetryCampaign_FilterByStep(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file) VALUES ('retry-step', 'active', 'seq.yml')`)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('a@b.com', 'A', 'B', 'b.com')")

	pastTime := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 1, ?, 'failed')`, pastTime)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, ?, 'failed')`, pastTime)

	step := 1
	result, err := RetryCampaign(db, "retry-step", &step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Retried != 1 {
		t.Errorf("expected 1 retried (step 1 only), got %d", result.Retried)
	}

	// Step 1 should be pending, step 2 should still be failed
	var s1, s2 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE step_number = 1").Scan(&s1)
	db.QueryRow("SELECT status FROM scheduled_sends WHERE step_number = 2").Scan(&s2)
	if s1 != "pending" {
		t.Errorf("step 1 should be 'pending', got %q", s1)
	}
	if s2 != "failed" {
		t.Errorf("step 2 should still be 'failed', got %q", s2)
	}
}

func TestRetryCampaign_NoFailed(t *testing.T) {
	db := testDB(t)
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file) VALUES ('no-fails', 'active', 'seq.yml')`)

	result, err := RetryCampaign(db, "no-fails", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Retried != 0 {
		t.Errorf("expected 0 retried, got %d", result.Retried)
	}
}

func TestFormatSendDays(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1,2,3,4,5", "Mon-Fri"},
		{"2,4", "Tue,Thu"},
		{"0,6", "Sun,Sat"},
		{"1,2,3", "Mon-Wed"},
	}
	for _, tt := range tests {
		got := FormatSendDays(tt.input)
		if got != tt.want {
			t.Errorf("FormatSendDays(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCampaignStateTransition_ActivateDraft(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('c1', 'draft', 'seq.yml')")

	err := CampaignStateTransition(db, "c1", "activate", "draft", "active")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var status string
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'c1'").Scan(&status)
	if status != "active" {
		t.Errorf("expected 'active', got %q", status)
	}
}

func TestCampaignStateTransition_PauseActive(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('c1', 'active', 'seq.yml')")

	err := CampaignStateTransition(db, "c1", "pause", "active", "paused")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var status string
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'c1'").Scan(&status)
	if status != "paused" {
		t.Errorf("expected 'paused', got %q", status)
	}
}

func TestCampaignStateTransition_ResumesPaused(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('c1', 'paused', 'seq.yml')")

	err := CampaignStateTransition(db, "c1", "resume", "paused", "active")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var status string
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'c1'").Scan(&status)
	if status != "active" {
		t.Errorf("expected 'active', got %q", status)
	}
}

func TestCampaignStateTransition_WrongState(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('c1', 'active', 'seq.yml')")

	// Can't activate an already-active campaign
	err := CampaignStateTransition(db, "c1", "activate", "draft", "active")
	if err == nil {
		t.Error("expected error for wrong state transition")
	}
	if !strings.Contains(err.Error(), "current status is \"active\"") {
		t.Errorf("expected error mentioning current status, got: %v", err)
	}
}

func TestCampaignStateTransition_NotFound(t *testing.T) {
	db := testDB(t)

	err := CampaignStateTransition(db, "nope", "activate", "draft", "active")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestDeleteCampaign(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('to-delete', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	sendAt := time.Now().UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at) VALUES (1, 1, 1, 1, ?)", sendAt)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 1, ?)", sendAt)

	id, err := DeleteCampaign(db, "to-delete")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 1 {
		t.Errorf("expected campaign id 1, got %d", id)
	}

	// Verify everything is gone
	var count int
	db.QueryRow("SELECT COUNT(*) FROM campaigns WHERE name = 'to-delete'").Scan(&count)
	if count != 0 {
		t.Error("campaign should be deleted")
	}
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE campaign_id = 1").Scan(&count)
	if count != 0 {
		t.Error("scheduled_sends should be deleted")
	}
	db.QueryRow("SELECT COUNT(*) FROM events WHERE campaign_id = 1").Scan(&count)
	if count != 0 {
		t.Error("events should be deleted")
	}
	db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = 1").Scan(&count)
	if count != 0 {
		t.Error("campaign_leads should be deleted")
	}
	db.QueryRow("SELECT COUNT(*) FROM campaign_accounts WHERE campaign_id = 1").Scan(&count)
	if count != 0 {
		t.Error("campaign_accounts should be deleted")
	}

	// Lead should still exist (not cascade deleted)
	db.QueryRow("SELECT COUNT(*) FROM leads WHERE email = 'a@x.com'").Scan(&count)
	if count != 1 {
		t.Error("lead should still exist after campaign delete")
	}
}

func TestDeleteCampaign_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := DeleteCampaign(db, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestGetCampaignPreview(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('preview-test', 'draft', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")

	sendAt1 := time.Now().UTC().Format(time.RFC3339)
	sendAt2 := time.Now().Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at, status) VALUES (1, 1, 1, 1, 0, ?, 'pending')", sendAt1)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at, status) VALUES (1, 2, 1, 1, 0, ?, 'pending')", sendAt1)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at, status) VALUES (1, 1, 1, 2, 0, ?, 'pending')", sendAt2)

	campaignID, status, preview, err := GetCampaignPreview(db, "preview-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if campaignID != 1 {
		t.Errorf("expected campaign id 1, got %d", campaignID)
	}
	if status != "draft" {
		t.Errorf("expected status 'draft', got %q", status)
	}
	if len(preview) != 3 {
		t.Fatalf("expected 3 preview rows, got %d", len(preview))
	}

	// Verify fields
	if preview[0].LeadEmail != "a@x.com" && preview[0].LeadEmail != "b@x.com" {
		t.Errorf("unexpected lead email: %q", preview[0].LeadEmail)
	}
	if preview[0].AccountEmail != "sender@x.com" {
		t.Errorf("expected account 'sender@x.com', got %q", preview[0].AccountEmail)
	}
	if preview[0].Status != "pending" {
		t.Errorf("expected status 'pending', got %q", preview[0].Status)
	}
}

func TestGetCampaignPreview_NotFound(t *testing.T) {
	db := testDB(t)

	_, _, _, err := GetCampaignPreview(db, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestGetCampaignPreview_Empty(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('empty', 'draft', 'seq.yml')")

	_, _, preview, err := GetCampaignPreview(db, "empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(preview) != 0 {
		t.Errorf("expected 0 rows, got %d", len(preview))
	}
}

func TestGetCampaignStatus(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, timezone, send_window_start, send_window_end)
		VALUES ('status-test', 'active', 'seq.yml', 'America/New_York', '09:00', '17:00')`)
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	sendAt := time.Now().UTC().Format(time.RFC3339)
	sentAt := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status, sent_at) VALUES (1, 1, 1, 1, ?, 'sent', ?)", sendAt, sentAt)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, ?, 'pending')", sendAt)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 1, ?)", sentAt)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'reply', 1, ?)", sendAt)

	info, err := GetCampaignStatus(db, "status-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Name != "status-test" {
		t.Errorf("expected name 'status-test', got %q", info.Name)
	}
	if info.Status != "active" {
		t.Errorf("expected status 'active', got %q", info.Status)
	}
	if info.Leads != 1 {
		t.Errorf("expected 1 lead, got %d", info.Leads)
	}
	if info.Accounts != 1 {
		t.Errorf("expected 1 account, got %d", info.Accounts)
	}
	if info.TotalSends != 2 {
		t.Errorf("expected 2 total sends, got %d", info.TotalSends)
	}
	if info.SendCounts["sent"] != 1 {
		t.Errorf("expected 1 sent, got %d", info.SendCounts["sent"])
	}
	if info.SendCounts["pending"] != 1 {
		t.Errorf("expected 1 pending, got %d", info.SendCounts["pending"])
	}
	if info.ReplyRate == nil {
		t.Error("expected reply rate to be set")
	} else if *info.ReplyRate != 100.0 {
		t.Errorf("expected 100%% reply rate (1 reply / 1 sent), got %.1f%%", *info.ReplyRate)
	}
	if info.NextSendAt == nil {
		t.Error("expected next_send_at to be set")
	}
	if info.LastSendAt == nil {
		t.Error("expected last_send_at to be set")
	}
	if info.SendWindow != "09:00 - 17:00" {
		t.Errorf("expected send window '09:00 - 17:00', got %q", info.SendWindow)
	}
}

func TestGetCampaignStatus_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := GetCampaignStatus(db, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestListCampaigns(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end, send_days)
		VALUES ('c1', 'active', 'seq.yml', '09:00', '17:00', '1,2,3,4,5')`)
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end, send_days)
		VALUES ('c2', 'draft', 'seq.yml', '10:00', '16:00', '1,2,3')`)
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	sendAt := time.Now().UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at) VALUES (1, 1, 1, 1, ?)", sendAt)
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at) VALUES (1, 1, 1, 2, ?)", sendAt)

	campaigns, err := ListCampaigns(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(campaigns) != 2 {
		t.Fatalf("expected 2 campaigns, got %d", len(campaigns))
	}

	// Campaigns are ordered by id DESC, so c2 first
	if campaigns[0].Name != "c2" {
		t.Errorf("expected first campaign 'c2', got %q", campaigns[0].Name)
	}
	if campaigns[0].Status != "draft" {
		t.Errorf("expected status 'draft', got %q", campaigns[0].Status)
	}
	if campaigns[0].SendDays != "Mon-Wed" {
		t.Errorf("expected 'Mon-Wed', got %q", campaigns[0].SendDays)
	}

	if campaigns[1].Name != "c1" {
		t.Errorf("expected second campaign 'c1', got %q", campaigns[1].Name)
	}
	if campaigns[1].Leads != 1 {
		t.Errorf("expected 1 lead, got %d", campaigns[1].Leads)
	}
	if campaigns[1].Sends != 2 {
		t.Errorf("expected 2 sends, got %d", campaigns[1].Sends)
	}
	if campaigns[1].SendDays != "Mon-Fri" {
		t.Errorf("expected 'Mon-Fri', got %q", campaigns[1].SendDays)
	}
}

func TestListCampaigns_Empty(t *testing.T) {
	db := testDB(t)

	campaigns, err := ListCampaigns(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(campaigns) != 0 {
		t.Errorf("expected 0 campaigns, got %d", len(campaigns))
	}
}

func TestCreateCampaign(t *testing.T) {
	db := testDB(t)

	// Set up temp dir with config, sequence, and leads files
	tmpDir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", tmpDir)

	configContent := `default_timezone: UTC
default_daily_limit: 50
min_gap_seconds: 90
max_gap_seconds: 140
send_window_start: "09:00"
send_window_end: "17:00"
send_days: "1,2,3,4,5"
`
	os.WriteFile(tmpDir+"/config.yml", []byte(configContent), 0644)

	seqContent := `name: Test
defaults:
  from_name: Tester
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
  - step: 2
    delay: 3
    subject: ""
    body: "Follow up {{first_name}}"
`
	seqFile := tmpDir + "/seq.yml"
	os.WriteFile(seqFile, []byte(seqContent), 0644)

	leadsContent := `email,first_name,company
alice@acme.com,Alice,Acme Corp
bob@bigco.com,Bob,BigCo
`
	leadsFile := tmpDir + "/leads.csv"
	os.WriteFile(leadsFile, []byte(leadsContent), 0644)

	// Insert account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	result, err := CreateCampaign(db, CreateCampaignOpts{
		Name:          "test-campaign",
		SequenceFile:  seqFile,
		LeadsFile:     leadsFile,
		AccountEmails: []string{"sender@x.com"},
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}

	if result.Name != "test-campaign" {
		t.Errorf("expected name 'test-campaign', got %q", result.Name)
	}
	if result.Status != "draft" {
		t.Errorf("expected status 'draft', got %q", result.Status)
	}
	if result.Leads != 2 {
		t.Errorf("expected 2 leads, got %d", result.Leads)
	}
	if result.Accounts != 1 {
		t.Errorf("expected 1 account, got %d", result.Accounts)
	}
	// 2 leads x 2 steps = 4 scheduled sends
	if result.ScheduledSends != 4 {
		t.Errorf("expected 4 scheduled sends, got %d", result.ScheduledSends)
	}

	// Verify DB state
	var status string
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'test-campaign'").Scan(&status)
	if status != "draft" {
		t.Errorf("expected campaign status 'draft', got %q", status)
	}

	var seqDB string
	db.QueryRow("SELECT sequence_content FROM campaigns WHERE name = 'test-campaign'").Scan(&seqDB)
	if !strings.Contains(seqDB, "Hi {{first_name}}") {
		t.Error("expected sequence_content to be stored in DB")
	}

	var sendCount int
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE campaign_id = ?", result.ID).Scan(&sendCount)
	if sendCount != 4 {
		t.Errorf("expected 4 scheduled_sends rows, got %d", sendCount)
	}

	var leadCount int
	db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ?", result.ID).Scan(&leadCount)
	if leadCount != 2 {
		t.Errorf("expected 2 campaign_leads rows, got %d", leadCount)
	}
}

func TestCreateCampaign_DuplicateName(t *testing.T) {
	db := testDB(t)
	tmpDir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", tmpDir)

	os.WriteFile(tmpDir+"/config.yml", []byte("default_timezone: UTC\ndefault_daily_limit: 50\nmin_gap_seconds: 90\nmax_gap_seconds: 140\nsend_window_start: \"09:00\"\nsend_window_end: \"17:00\"\nsend_days: \"1,2,3,4,5\"\n"), 0644)

	seqContent := "name: Test\ndefaults:\n  from_name: X\nsteps:\n  - step: 1\n    delay: 0\n    subject: \"Hi\"\n    body: \"Hello\"\n"
	seqFile := tmpDir + "/seq.yml"
	os.WriteFile(seqFile, []byte(seqContent), 0644)

	leadsContent := "email\nalice@acme.com\n"
	leadsFile := tmpDir + "/leads.csv"
	os.WriteFile(leadsFile, []byte(leadsContent), 0644)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	_, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "dup", SequenceFile: seqFile, LeadsFile: leadsFile, AccountEmails: []string{"sender@x.com"},
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err = CreateCampaign(db, CreateCampaignOpts{
		Name: "dup", SequenceFile: seqFile, LeadsFile: leadsFile, AccountEmails: []string{"sender@x.com"},
	})
	if err == nil {
		t.Error("expected error for duplicate campaign name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

func TestCreateCampaign_BadAccount(t *testing.T) {
	db := testDB(t)
	tmpDir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", tmpDir)

	os.WriteFile(tmpDir+"/config.yml", []byte("default_timezone: UTC\ndefault_daily_limit: 50\nmin_gap_seconds: 90\nmax_gap_seconds: 140\nsend_window_start: \"09:00\"\nsend_window_end: \"17:00\"\nsend_days: \"1,2,3,4,5\"\n"), 0644)

	seqFile := tmpDir + "/seq.yml"
	os.WriteFile(seqFile, []byte("name: Test\ndefaults:\n  from_name: X\nsteps:\n  - step: 1\n    delay: 0\n    subject: \"Hi\"\n    body: \"Hello\"\n"), 0644)

	leadsFile := tmpDir + "/leads.csv"
	os.WriteFile(leadsFile, []byte("email\na@x.com\n"), 0644)

	_, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "bad-acct", SequenceFile: seqFile, LeadsFile: leadsFile, AccountEmails: []string{"nonexistent@x.com"},
	})
	if err == nil {
		t.Error("expected error for non-existent account")
	}
}

func TestCreateCampaign_SkipsBlacklistedLeads(t *testing.T) {
	db := testDB(t)
	tmpDir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", tmpDir)

	os.WriteFile(tmpDir+"/config.yml", []byte("default_timezone: UTC\ndefault_daily_limit: 50\nmin_gap_seconds: 90\nmax_gap_seconds: 140\nsend_window_start: \"09:00\"\nsend_window_end: \"17:00\"\nsend_days: \"1,2,3,4,5\"\n"), 0644)

	seqFile := tmpDir + "/seq.yml"
	os.WriteFile(seqFile, []byte("name: Test\ndefaults:\n  from_name: X\nsteps:\n  - step: 1\n    delay: 0\n    subject: \"Hi\"\n    body: \"Hello\"\n"), 0644)

	leadsFile := tmpDir + "/leads.csv"
	os.WriteFile(leadsFile, []byte("email\nalice@x.com\nbob@x.com\n"), 0644)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	// Pre-blacklist bob
	db.Exec("INSERT INTO leads (email, domain, global_status) VALUES ('bob@x.com', 'x.com', 'blacklisted')")

	result, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "skip-test", SequenceFile: seqFile, LeadsFile: leadsFile, AccountEmails: []string{"sender@x.com"},
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if result.Leads != 1 {
		t.Errorf("expected 1 lead (bob should be skipped), got %d", result.Leads)
	}
}

func TestCreateCampaign_WithStartDate(t *testing.T) {
	db := testDB(t)
	tmpDir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", tmpDir)

	os.WriteFile(tmpDir+"/config.yml", []byte("default_timezone: UTC\ndefault_daily_limit: 50\nmin_gap_seconds: 90\nmax_gap_seconds: 140\nsend_window_start: \"09:00\"\nsend_window_end: \"17:00\"\nsend_days: \"0,1,2,3,4,5,6\"\n"), 0644)

	seqFile := tmpDir + "/seq.yml"
	os.WriteFile(seqFile, []byte("name: Test\ndefaults:\n  from_name: X\nsteps:\n  - step: 1\n    delay: 0\n    subject: \"Hi\"\n    body: \"Hello\"\n"), 0644)

	leadsFile := tmpDir + "/leads.csv"
	os.WriteFile(leadsFile, []byte("email\na@x.com\n"), 0644)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	result, err := CreateCampaign(db, CreateCampaignOpts{
		Name: "start-date", SequenceFile: seqFile, LeadsFile: leadsFile,
		AccountEmails: []string{"sender@x.com"}, StartDate: "2026-06-15",
	})
	if err != nil {
		t.Fatalf("CreateCampaign error: %v", err)
	}
	if result.ScheduledSends != 1 {
		t.Errorf("expected 1 send, got %d", result.ScheduledSends)
	}

	// Verify send_at is on the specified date
	var sendAt string
	db.QueryRow("SELECT send_at FROM scheduled_sends WHERE campaign_id = ?", result.ID).Scan(&sendAt)
	if !strings.Contains(sendAt, "2026-06-15") {
		t.Errorf("expected send_at on 2026-06-15, got %q", sendAt)
	}
}
