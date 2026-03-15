package internal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSequenceFromBytes_Basic(t *testing.T) {
	yaml := []byte(`
name: Test Sequence
defaults:
  from_name: "Anders"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
  - step: 2
    delay: 3
    body: "Following up..."
  - step: 3
    delay: 5
    body: "Last note"
`)
	seq, err := ParseSequenceFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if seq.Name != "Test Sequence" {
		t.Errorf("expected name 'Test Sequence', got %q", seq.Name)
	}
	if len(seq.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(seq.Steps))
	}
	if seq.Steps[0].Subject != "Hi {{first_name}}" {
		t.Errorf("unexpected step 1 subject: %s", seq.Steps[0].Subject)
	}
	if seq.Steps[1].Delay != 3 {
		t.Errorf("expected step 2 delay=3, got %d", seq.Steps[1].Delay)
	}
}

func TestParseSequenceFromBytes_NoSteps(t *testing.T) {
	yaml := []byte(`name: Empty`)
	_, err := ParseSequenceFromBytes(yaml)
	if err == nil {
		t.Fatal("expected error for no steps")
	}
}

func TestParseSequenceFromBytes_NoBody(t *testing.T) {
	yaml := []byte(`
steps:
  - step: 1
    subject: "Hi"
`)
	_, err := ParseSequenceFromBytes(yaml)
	if err == nil {
		t.Fatal("expected error for step with no body")
	}
}

func TestParseSequenceFromBytes_Step1NoSubject(t *testing.T) {
	yaml := []byte(`
steps:
  - step: 1
    body: "Hello"
`)
	_, err := ParseSequenceFromBytes(yaml)
	if err == nil {
		t.Fatal("expected error for step 1 with no subject")
	}
}

func TestParseSequenceFromBytes_Step1WithVariants(t *testing.T) {
	// Step 1 with no base subject but has variants with subjects — should be OK
	yaml := []byte(`
steps:
  - step: 1
    body: "Hello"
    variants:
      - subject: "Alt subject"
        body: "Alt body"
`)
	seq, err := ParseSequenceFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(seq.Steps[0].Variants) != 1 {
		t.Errorf("expected 1 variant, got %d", len(seq.Steps[0].Variants))
	}
}

func TestParseSequenceFromBytes_Variants(t *testing.T) {
	yaml := []byte(`
steps:
  - step: 1
    subject: "Subject A"
    body: "Body A"
    variants:
      - subject: "Subject B"
        body: "Body B"
      - subject: "Subject C"
        body: "Body C"
`)
	seq, err := ParseSequenceFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(seq.Steps[0].Variants) != 2 {
		t.Errorf("expected 2 variants, got %d", len(seq.Steps[0].Variants))
	}
}

func TestCollectPlaceholders(t *testing.T) {
	yaml := []byte(`
steps:
  - step: 1
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
    variants:
      - subject: "{{company}} intro"
        body: "Hi {{last_name}}"
  - step: 2
    delay: 3
    body: "{{first_name}}, following up about {{company}}"
`)
	seq, err := ParseSequenceFromBytes(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	placeholders := seq.CollectPlaceholders()

	expected := map[string]bool{
		"first_name": true,
		"company":    true,
		"last_name":  true,
	}
	if len(placeholders) != len(expected) {
		t.Fatalf("expected %d placeholders, got %d: %v", len(expected), len(placeholders), placeholders)
	}
	for _, p := range placeholders {
		if !expected[p] {
			t.Errorf("unexpected placeholder: %s", p)
		}
	}
}

func TestComputeSchedule_Basic(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Delay: 0, Subject: "Hi", Body: "Hello"},
			{Step: 2, Delay: 3, Body: "Follow up"},
			{Step: 3, Delay: 5, Body: "Last"},
		},
	}

	tz, _ := time.LoadLocation("America/New_York")
	// Start on a Monday at 10:00 AM
	startTime := time.Date(2025, 1, 6, 10, 0, 0, 0, tz) // Monday

	rows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{100, 200},
		Leads:           []LeadForSchedule{{ID: 1}, {ID: 2}, {ID: 3}},
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90, // fixed gap for deterministic test
		StartTime:       startTime,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 leads × 3 steps = 9 rows
	if len(rows) != 9 {
		t.Fatalf("expected 9 rows, got %d", len(rows))
	}

	// Check round-robin: lead 1→account 100, lead 2→account 200, lead 3→account 100
	if rows[0].AccountID != 100 {
		t.Errorf("lead 1 should use account 100, got %d", rows[0].AccountID)
	}
	if rows[3].AccountID != 200 {
		t.Errorf("lead 2 should use account 200, got %d", rows[3].AccountID)
	}
	if rows[6].AccountID != 100 {
		t.Errorf("lead 3 should use account 100, got %d", rows[6].AccountID)
	}

	// All steps for one lead should use the same account
	for i := 0; i < 3; i++ {
		if rows[i].AccountID != rows[0].AccountID {
			t.Errorf("lead 1 step %d: expected account %d, got %d", i+1, rows[0].AccountID, rows[i].AccountID)
		}
	}

	// Step 2 should be 3 days after step 1
	step1Time := rows[0].SendAt
	step2Time := rows[1].SendAt
	diff := step2Time.Sub(step1Time)
	if diff < 3*24*time.Hour {
		t.Errorf("step 2 should be at least 3 days after step 1, got %v", diff)
	}

	// Step 3 should be 5 days after step 2
	step3Time := rows[2].SendAt
	diff = step3Time.Sub(step2Time)
	if diff < 5*24*time.Hour {
		t.Errorf("step 3 should be at least 5 days after step 2, got %v", diff)
	}
}

func TestComputeSchedule_WeekendSkipping(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Delay: 0, Subject: "Hi", Body: "Hello"},
			{Step: 2, Delay: 1, Body: "Follow up"}, // 1 day later
		},
	}

	tz, _ := time.LoadLocation("America/New_York")
	// Start on a Friday at 10:00 AM
	startTime := time.Date(2025, 1, 10, 10, 0, 0, 0, tz) // Friday

	rows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{100},
		Leads:           []LeadForSchedule{{ID: 1}},
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       startTime,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step 1 on Friday
	step1Day := rows[0].SendAt.In(tz).Weekday()
	if step1Day != time.Friday {
		t.Errorf("step 1 should be Friday, got %s", step1Day)
	}

	// Step 2: Friday + 1 day = Saturday → should skip to Monday
	step2Day := rows[1].SendAt.In(tz).Weekday()
	if step2Day != time.Monday {
		t.Errorf("step 2 should skip weekend to Monday, got %s", step2Day)
	}
}

func TestComputeSchedule_WindowClamping(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Delay: 0, Subject: "Hi", Body: "Hello"},
		},
	}

	tz, _ := time.LoadLocation("America/New_York")
	// Start at 7:00 AM — before 9 AM window
	startTime := time.Date(2025, 1, 6, 7, 0, 0, 0, tz) // Monday 7 AM

	rows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{100},
		Leads:           []LeadForSchedule{{ID: 1}},
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       startTime,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be clamped to 9:00 AM
	sendHour := rows[0].SendAt.In(tz).Hour()
	if sendHour < 9 {
		t.Errorf("expected send_at hour >= 9, got %d", sendHour)
	}
}

func TestComputeSchedule_WindowOverflow(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Delay: 0, Subject: "Hi", Body: "Hello"},
		},
	}

	tz, _ := time.LoadLocation("America/New_York")
	// Start at 18:00 — after 17:00 window end
	startTime := time.Date(2025, 1, 6, 18, 0, 0, 0, tz) // Monday 6 PM

	rows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{100},
		Leads:           []LeadForSchedule{{ID: 1}},
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       startTime,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be pushed to next day's window start (Tuesday 9 AM)
	sendTime := rows[0].SendAt.In(tz)
	if sendTime.Weekday() != time.Tuesday {
		t.Errorf("expected Tuesday, got %s", sendTime.Weekday())
	}
	if sendTime.Hour() != 9 {
		t.Errorf("expected hour 9, got %d", sendTime.Hour())
	}
}

func TestComputeSchedule_VariantAssignment(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{
				Step: 1, Delay: 0,
				Subject: "Subject A", Body: "Body A",
				Variants: []SequenceVariant{
					{Subject: "Subject B", Body: "Body B"},
					{Subject: "Subject C", Body: "Body C"},
				},
			},
		},
	}

	tz, _ := time.LoadLocation("UTC")
	startTime := time.Date(2025, 1, 6, 10, 0, 0, 0, tz)

	// 6 leads, 3 variants (base + 2) → round-robin: 0,1,2,0,1,2
	var leads []LeadForSchedule
	for i := 1; i <= 6; i++ {
		leads = append(leads, LeadForSchedule{ID: int64(i)})
	}

	rows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{100},
		Leads:           leads,
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       startTime,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedVariants := []int{0, 1, 2, 0, 1, 2}
	for i, row := range rows {
		if row.VariantIndex != expectedVariants[i] {
			t.Errorf("lead %d: expected variant_index %d, got %d", i+1, expectedVariants[i], row.VariantIndex)
		}
	}
}

func TestComputeSchedule_NoAccounts(t *testing.T) {
	seq := &Sequence{Steps: []SequenceStep{{Step: 1, Subject: "Hi", Body: "Hello"}}}
	tz, _ := time.LoadLocation("UTC")
	_, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      nil,
		Leads:           []LeadForSchedule{{ID: 1}},
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       time.Now(),
	})
	if err == nil {
		t.Fatal("expected error for no accounts")
	}
}

func TestComputeSchedule_NoLeads(t *testing.T) {
	seq := &Sequence{Steps: []SequenceStep{{Step: 1, Subject: "Hi", Body: "Hello"}}}
	tz, _ := time.LoadLocation("UTC")
	_, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      1,
		AccountIDs:      []int64{1},
		Leads:           nil,
		Sequence:        seq,
		SendWindowStart: "09:00",
		SendWindowEnd:   "17:00",
		SendDays:        []time.Weekday{time.Monday},
		Timezone:        tz,
		MinGapSeconds:   90,
		MaxGapSeconds:   90,
		StartTime:       time.Now(),
	})
	if err == nil {
		t.Fatal("expected error for no leads")
	}
}

func TestValidateLeadFields(t *testing.T) {
	records := []LeadRecord{
		{Fields: map[string]string{"email": "john@acme.com", "first_name": "John", "company": "Acme"}},
		{Fields: map[string]string{"email": "jane@foo.com", "first_name": "Jane", "company": ""}},
	}

	// Should pass — only checking first_name
	err := ValidateLeadFields(records, []string{"first_name"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should fail — jane missing company
	err = ValidateLeadFields(records, []string{"first_name", "company"})
	if err == nil {
		t.Fatal("expected error for missing company")
	}
	if !contains(err.Error(), "jane@foo.com") {
		t.Errorf("error should mention jane: %v", err)
	}
	if !contains(err.Error(), "company") {
		t.Errorf("error should mention company: %v", err)
	}
}

func TestValidateLeadFields_MissingKey(t *testing.T) {
	records := []LeadRecord{
		{Fields: map[string]string{"email": "john@acme.com", "first_name": "John"}},
	}

	err := ValidateLeadFields(records, []string{"first_name", "title"})
	if err == nil {
		t.Fatal("expected error for missing title field")
	}
}

func TestParseSendDays(t *testing.T) {
	days, err := ParseSendDays("1,2,3,4,5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(days) != 5 {
		t.Fatalf("expected 5 days, got %d", len(days))
	}
	if days[0] != time.Monday {
		t.Errorf("expected Monday, got %s", days[0])
	}
	if days[4] != time.Friday {
		t.Errorf("expected Friday, got %s", days[4])
	}
}

func TestParseSendDays_Invalid(t *testing.T) {
	_, err := ParseSendDays("1,2,7")
	if err == nil {
		t.Fatal("expected error for day 7")
	}
}

func TestCampaignStateTransitions_DB(t *testing.T) {
	db := testDB(t)

	// Insert a draft campaign
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('test', 'seq.yml')")

	// draft → active (valid)
	var status string
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'test'").Scan(&status)
	if status != "draft" {
		t.Fatalf("expected draft, got %s", status)
	}

	db.Exec("UPDATE campaigns SET status = 'active' WHERE name = 'test' AND status = 'draft'")
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'test'").Scan(&status)
	if status != "active" {
		t.Errorf("expected active, got %s", status)
	}

	// active → paused (valid)
	db.Exec("UPDATE campaigns SET status = 'paused' WHERE name = 'test' AND status = 'active'")
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'test'").Scan(&status)
	if status != "paused" {
		t.Errorf("expected paused, got %s", status)
	}

	// paused → active (valid)
	db.Exec("UPDATE campaigns SET status = 'active' WHERE name = 'test' AND status = 'paused'")
	db.QueryRow("SELECT status FROM campaigns WHERE name = 'test'").Scan(&status)
	if status != "active" {
		t.Errorf("expected active, got %s", status)
	}

	// active → draft (invalid — should not change)
	res, _ := db.Exec("UPDATE campaigns SET status = 'draft' WHERE name = 'test' AND status = 'paused'")
	affected, _ := res.RowsAffected()
	if affected != 0 {
		t.Error("should not transition active → draft via paused check")
	}
}

func TestUpdateCampaign_Validation(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('test', 'seq.yml')")

	// Invalid timezone
	badTZ := "Invalid/Timezone"
	err := UpdateCampaign(db, "test", UpdateCampaignOpts{Timezone: &badTZ})
	if err == nil {
		t.Error("expected error for invalid timezone")
	}

	// Invalid send window start
	badTime := "25:00"
	err = UpdateCampaign(db, "test", UpdateCampaignOpts{SendWindowStart: &badTime})
	if err == nil {
		t.Error("expected error for invalid send_window_start")
	}

	// Invalid send days
	badDays := "8,9"
	err = UpdateCampaign(db, "test", UpdateCampaignOpts{SendDays: &badDays})
	if err == nil {
		t.Error("expected error for invalid send_days")
	}

	// Valid update should succeed
	validTZ := "America/New_York"
	validStart := "08:00"
	err = UpdateCampaign(db, "test", UpdateCampaignOpts{
		Timezone:        &validTZ,
		SendWindowStart: &validStart,
	})
	if err != nil {
		t.Errorf("expected no error for valid update, got: %v", err)
	}

	// Verify values were updated
	var tz, ws string
	db.QueryRow("SELECT timezone, send_window_start FROM campaigns WHERE name = 'test'").Scan(&tz, &ws)
	if tz != "America/New_York" {
		t.Errorf("expected timezone 'America/New_York', got %q", tz)
	}
	if ws != "08:00" {
		t.Errorf("expected send_window_start '08:00', got %q", ws)
	}

	// Campaign not found
	err = UpdateCampaign(db, "nonexistent", UpdateCampaignOpts{Timezone: &validTZ})
	if err == nil {
		t.Error("expected error for nonexistent campaign")
	}
}

// writeTempCSV creates a temp CSV file and returns its path.
func writeTempCSV(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "leads.csv")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp CSV: %v", err)
	}
	return path
}

func TestCloneCampaign(t *testing.T) {
	db := testDB(t)

	// Setup source campaign
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	seqYAML := `name: Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}} at {{company}}"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone, min_gap_seconds, max_gap_seconds,
		stop_on_reply, stop_on_domain_reply)
		VALUES ('source', 'active', 'seq.yml', ?, '08:00', '16:00', '1,2,3,4,5', 'America/New_York', 100, 150, 1, 1)`,
		seqYAML)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	// Create temp CSV
	csvPath := writeTempCSV(t, "email,first_name,company\nalice@new.com,Alice,NewCo\nbob@new.com,Bob,NewCo\n")

	// Clone without specifying accounts (reuse source)
	result, err := CloneCampaign(db, CloneCampaignOpts{
		SourceName: "source",
		NewName:    "cloned",
		LeadsFile:  csvPath,
	})
	if err != nil {
		t.Fatalf("CloneCampaign error: %v", err)
	}

	if result.Name != "cloned" {
		t.Errorf("expected name 'cloned', got %q", result.Name)
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

	// Verify settings were copied
	var windowStart, windowEnd, sendDays, tz string
	var minGap, maxGap, stopOnReply, stopOnDomain int
	db.QueryRow(`SELECT send_window_start, send_window_end, send_days, timezone,
		min_gap_seconds, max_gap_seconds, stop_on_reply, stop_on_domain_reply
		FROM campaigns WHERE name = 'cloned'`).
		Scan(&windowStart, &windowEnd, &sendDays, &tz, &minGap, &maxGap, &stopOnReply, &stopOnDomain)

	if windowStart != "08:00" {
		t.Errorf("expected window start '08:00', got %q", windowStart)
	}
	if tz != "America/New_York" {
		t.Errorf("expected timezone 'America/New_York', got %q", tz)
	}
	if stopOnDomain != 1 {
		t.Errorf("expected stop_on_domain_reply=1, got %d", stopOnDomain)
	}
	if minGap != 100 {
		t.Errorf("expected min_gap=100, got %d", minGap)
	}
}

func TestCloneCampaign_DuplicateName(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content)
		VALUES ('source', 'active', 'seq.yml', 'name: Test
defaults:
  from_name: "T"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello"
')`)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	csvPath := writeTempCSV(t, "email,first_name\nalice@new.com,Alice\n")

	// Clone to existing name should fail
	_, err := CloneCampaign(db, CloneCampaignOpts{
		SourceName: "source",
		NewName:    "source",
		LeadsFile:  csvPath,
	})
	if err == nil {
		t.Error("expected error for duplicate campaign name")
	}
}

func TestAddLeadsToCampaign(t *testing.T) {
	db := testDB(t)

	// Setup campaign with 1 existing lead
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	seqYAML := `name: Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}}"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone)
		VALUES ('test-add', 'active', 'seq.yml', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`, seqYAML)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('existing@acme.com', 'Existing', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	// CSV with 3 leads: 1 existing (should skip), 2 new
	csvPath := writeTempCSV(t, "email,first_name\nexisting@acme.com,Existing\nalice@new.com,Alice\nbob@new.com,Bob\n")

	result, err := AddLeadsToCampaign(db, "test-add", csvPath)
	if err != nil {
		t.Fatalf("AddLeadsToCampaign error: %v", err)
	}

	if result.LeadsAdded != 2 {
		t.Errorf("expected 2 leads added, got %d", result.LeadsAdded)
	}
	if result.LeadsSkipped != 1 {
		t.Errorf("expected 1 lead skipped, got %d", result.LeadsSkipped)
	}
	if result.ScheduledSends < 2 {
		t.Errorf("expected at least 2 scheduled sends, got %d", result.ScheduledSends)
	}

	// Verify total leads in campaign
	var totalLeads int
	db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = 1").Scan(&totalLeads)
	if totalLeads != 3 {
		t.Errorf("expected 3 total leads in campaign, got %d", totalLeads)
	}
}

func TestAddLeadsToCampaign_SkipsBlacklisted(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	seqYAML := `name: Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content,
		send_window_start, send_window_end, send_days, timezone)
		VALUES ('test-bl', 'draft', 'seq.yml', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`, seqYAML)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	// Pre-create a blacklisted lead
	db.Exec("INSERT INTO leads (email, first_name, domain, global_status) VALUES ('bad@acme.com', 'Bad', 'acme.com', 'blacklisted')")

	csvPath := writeTempCSV(t, "email,first_name\nbad@acme.com,Bad\ngood@acme.com,Good\n")

	result, err := AddLeadsToCampaign(db, "test-bl", csvPath)
	if err != nil {
		t.Fatalf("AddLeadsToCampaign error: %v", err)
	}

	if result.LeadsAdded != 1 {
		t.Errorf("expected 1 lead added (blacklisted skipped), got %d", result.LeadsAdded)
	}
	if result.LeadsSkipped != 1 {
		t.Errorf("expected 1 skipped, got %d", result.LeadsSkipped)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
