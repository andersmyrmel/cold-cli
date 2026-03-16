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

	rendered, err := GetCampaignRenderedPreview(db, "render-test")
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

func TestGetCampaignRenderedPreview_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := GetCampaignRenderedPreview(db, "nonexistent")
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
