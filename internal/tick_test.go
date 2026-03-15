package internal

import (
	"database/sql"
	"testing"
	"time"
)

// setupTickTestDB creates a test database with a full campaign setup for tick testing.
// Returns the db, campaign ID, and account/lead IDs.
func setupTickTestDB(t *testing.T) (*sql.DB, int64, []int64, []int64) {
	t.Helper()
	db := testDB(t)

	// Create account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Create campaign (active)
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end,
		send_days, timezone) VALUES ('test', 'active', 'testdata/seq.yml', '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`)

	// Create leads
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('john@acme.com', 'John', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('jane@acme.com', 'Jane', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('bob@other.com', 'Bob', 'Other', 'other.com')")

	// Create campaign_leads
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 2, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 3, 'active')")

	// Create campaign_accounts
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	return db, 1, []int64{1}, []int64{1, 2, 3}
}

func insertPendingSend(t *testing.T, db *sql.DB, campaignID, leadID, accountID int64, step int, sendAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (?, ?, ?, ?, ?, 'pending')`,
		campaignID, leadID, accountID, step, sendAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("inserting pending send: %v", err)
	}
}

func TestTick_SendsDueEmails(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Schedule a send in the past (due now)
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{
		DB:      db,
		GWS:     mock,
		DryRun:  false,
		Now:     now,
		NoSleep: true,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent, got %d", result.Sent)
	}
	if len(mock.SentEmails) != 1 {
		t.Errorf("expected 1 email sent via gws, got %d", len(mock.SentEmails))
	}

	// Verify send status updated
	var status string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&status)
	if status != "sent" {
		t.Errorf("expected status 'sent', got %q", status)
	}

	// Verify event created
	var eventCount int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'sent'").Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("expected 1 sent event, got %d", eventCount)
	}
}

func TestTick_SkipsFutureSends(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Schedule a send in the future (not yet due)
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 0 {
		t.Errorf("expected 0 sent (future send), got %d", result.Sent)
	}
	if len(mock.SentEmails) != 0 {
		t.Errorf("expected 0 emails sent, got %d", len(mock.SentEmails))
	}
}

func TestTick_DryRun(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{
		DB:      db,
		GWS:     mock,
		DryRun:  true,
		Now:     now,
		NoSleep: true,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent (dry-run), got %d", result.Sent)
	}
	if !result.DryRun {
		t.Error("expected DryRun=true")
	}
	// Should NOT have actually sent via gws
	if len(mock.SentEmails) != 0 {
		t.Errorf("expected 0 actual sends in dry-run, got %d", len(mock.SentEmails))
	}

	// Send status should still be pending
	var status string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&status)
	if status != "pending" {
		t.Errorf("expected status 'pending' after dry-run, got %q", status)
	}
}

func TestTick_FailedSendIsolation(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Two sends due
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))
	insertPendingSend(t, db, campaignID, leadIDs[1], accountIDs[0], 1, now.Add(-30*time.Minute))

	// Use a custom mock that fails on first call
	failOnFirst := &failFirstMockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: failOnFirst, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
	if result.Sent != 1 {
		t.Errorf("expected 1 sent, got %d", result.Sent)
	}

	// Verify first send is failed, second is sent
	var status1, status2 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE lead_id = ? AND campaign_id = ?", leadIDs[0], campaignID).Scan(&status1)
	db.QueryRow("SELECT status FROM scheduled_sends WHERE lead_id = ? AND campaign_id = ?", leadIDs[1], campaignID).Scan(&status2)

	if status1 != "failed" {
		t.Errorf("first send should be 'failed', got %q", status1)
	}
	if status2 != "sent" {
		t.Errorf("second send should be 'sent', got %q", status2)
	}
}


func TestTick_DailyLimitEnforcement(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Set daily limit to 1
	db.Exec("UPDATE accounts SET daily_limit = 1 WHERE id = ?", accountIDs[0])

	// Two sends due
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-2*time.Hour))
	insertPendingSend(t, db, campaignID, leadIDs[1], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	// Should send 1, skip 1 (daily limit reached after first send)
	if result.Sent != 1 {
		t.Errorf("expected 1 sent, got %d", result.Sent)
	}
	if result.Skipped != 1 {
		t.Errorf("expected 1 skipped (daily limit), got %d", result.Skipped)
	}
}

func TestTick_Step1BackfillsThreadID(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Step 1 due now, step 2 in the future
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 2, now.Add(72*time.Hour))

	mock := &MockGWS{
		SendMsgID:    "msg-abc",
		SendThreadID: "thread-xyz",
	}

	_, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	// Step 2's scheduled_send should now have thread_id and parent_message_id backfilled
	var threadID, parentMsgID string
	db.QueryRow(`SELECT thread_id, parent_message_id FROM scheduled_sends
		WHERE campaign_id = ? AND lead_id = ? AND step_number = 2`,
		campaignID, leadIDs[0]).Scan(&threadID, &parentMsgID)

	if threadID != "thread-xyz" {
		t.Errorf("expected thread_id 'thread-xyz', got %q", threadID)
	}
	if parentMsgID != "msg-abc" {
		t.Errorf("expected parent_message_id 'msg-abc', got %q", parentMsgID)
	}
}

func TestTick_ReplyDetection(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)

	// Insert a sent event (simulating step 1 was already sent)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (?, ?, ?, 'sent', 1, '<sent-msg-1@gmail.com>', 'thread-1')`,
		campaignID, leadIDs[0], accountIDs[0])

	// Insert pending step 2 send
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 2,
		time.Now().UTC().Add(72*time.Hour))

	// Mock inbox has a reply to our sent message
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:        "reply-msg-1",
				ThreadID:  "thread-1",
				InReplyTo: "<sent-msg-1@gmail.com>",
				From:      "john@acme.com",
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}

	if replies != 1 {
		t.Errorf("expected 1 reply detected, got %d", replies)
	}

	// Campaign_lead should be 'replied'
	var clStatus string
	db.QueryRow("SELECT status FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&clStatus)
	if clStatus != "replied" {
		t.Errorf("expected campaign_lead status 'replied', got %q", clStatus)
	}

	// Pending sends should be 'skipped'
	var ssStatus string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ? AND step_number = 2",
		campaignID, leadIDs[0]).Scan(&ssStatus)
	if ssStatus != "skipped" {
		t.Errorf("expected scheduled_send status 'skipped', got %q", ssStatus)
	}
}

func TestTick_DomainReplyCascade(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)

	// Enable stop_on_domain_reply
	db.Exec("UPDATE campaigns SET stop_on_domain_reply = 1 WHERE id = ?", campaignID)

	// Insert a sent event for lead 1 (john@acme.com)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (?, ?, ?, 'sent', 1, '<sent-to-john@gmail.com>', 'thread-john')`,
		campaignID, leadIDs[0], accountIDs[0])

	// Insert pending sends for lead 1 (step 2) and lead 2 (jane@acme.com, same domain)
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 2, time.Now().Add(72*time.Hour))
	insertPendingSend(t, db, campaignID, leadIDs[1], accountIDs[0], 1, time.Now().Add(1*time.Hour))

	// Lead 3 (bob@other.com) should NOT be affected
	insertPendingSend(t, db, campaignID, leadIDs[2], accountIDs[0], 1, time.Now().Add(1*time.Hour))

	// Mock reply from john@acme.com
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:        "reply-john",
				ThreadID:  "thread-john",
				InReplyTo: "<sent-to-john@gmail.com>",
				From:      "john@acme.com",
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	ProcessReplies(db, mock, accounts)

	// Lead 1's sends should be skipped (replied)
	var s1 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE lead_id = ? AND step_number = 2", leadIDs[0]).Scan(&s1)
	if s1 != "skipped" {
		t.Errorf("lead 1 step 2 should be 'skipped', got %q", s1)
	}

	// Lead 2's sends should be skipped (same domain: acme.com)
	var s2 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE lead_id = ?", leadIDs[1]).Scan(&s2)
	if s2 != "skipped" {
		t.Errorf("lead 2 (same domain) should be 'skipped', got %q", s2)
	}

	// Lead 3's sends should still be pending (different domain: other.com)
	var s3 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE lead_id = ?", leadIDs[2]).Scan(&s3)
	if s3 != "pending" {
		t.Errorf("lead 3 (different domain) should be 'pending', got %q", s3)
	}
}

func TestTick_BounceDetection(t *testing.T) {
	db, _, _, leadIDs := setupTickTestDB(t)

	// Mock bounce NDR mentioning john@acme.com
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:      "bounce-1",
				From:    "MAILER-DAEMON@google.com",
				Subject: "Delivery Status Notification",
				Snippet: "Delivery to john@acme.com has failed permanently",
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, err := ProcessBounces(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessBounces error: %v", err)
	}

	if bounces != 1 {
		t.Errorf("expected 1 bounce detected, got %d", bounces)
	}

	// Lead should be globally bounced
	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = ?", leadIDs[0]).Scan(&globalStatus)
	if globalStatus != "bounced" {
		t.Errorf("expected global_status 'bounced', got %q", globalStatus)
	}
}

func TestTick_InactiveCampaignIgnored(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Pause the campaign
	db.Exec("UPDATE campaigns SET status = 'paused' WHERE id = ?", campaignID)

	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	// Should not send anything for paused campaign
	if result.Sent != 0 {
		t.Errorf("expected 0 sent for paused campaign, got %d", result.Sent)
	}
}

func TestTick_SendWindowEnforcement(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)

	// Set send window to 09:00-17:00
	db.Exec("UPDATE campaigns SET send_window_start = '09:00', send_window_end = '17:00' WHERE id = ?", campaignID)

	// Set current time to 20:00 (outside window)
	now := time.Date(2025, 1, 6, 20, 0, 0, 0, time.UTC)
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Skipped != 1 {
		t.Errorf("expected 1 skipped (outside window), got %d", result.Skipped)
	}
	if result.Sent != 0 {
		t.Errorf("expected 0 sent, got %d", result.Sent)
	}
}

func TestFormatTickResult(t *testing.T) {
	tests := []struct {
		result   TickResult
		contains string
	}{
		{TickResult{}, "nothing to do"},
		{TickResult{Sent: 3}, "3 sent"},
		{TickResult{Failed: 1}, "1 failed"},
		{TickResult{DryRun: true, Sent: 2}, "[DRY RUN]"},
		{TickResult{Sent: 1, RepliesDetected: 2, BouncesDetected: 1}, "1 sent"},
	}

	for _, tt := range tests {
		got := FormatTickResult(&tt.result)
		if !containsStr(got, tt.contains) {
			t.Errorf("FormatTickResult(%+v) = %q, want to contain %q", tt.result, got, tt.contains)
		}
	}
}

func TestExtractBouncedEmail(t *testing.T) {
	tests := []struct {
		snippet string
		subject string
		want    string
	}{
		{"Delivery to john@acme.com has failed", "", "john@acme.com"},
		{"", "Mail delivery failed: returning message to john@acme.com", "john@acme.com"},
		{"no email here", "also none here", ""},
		{"Address not found: <JANE@FOO.COM>", "", "jane@foo.com"},
	}

	for _, tt := range tests {
		got := extractBouncedEmail(tt.snippet, tt.subject)
		if got != tt.want {
			t.Errorf("extractBouncedEmail(%q, %q) = %q, want %q", tt.snippet, tt.subject, got, tt.want)
		}
	}
}
