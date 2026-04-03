package internal

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
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

func insertPendingFollowUpSend(t *testing.T, db *sql.DB, campaignID, leadID, accountID int64, variantIndex int, sendAt time.Time, threadID, parentMessageID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO scheduled_sends (
			campaign_id, lead_id, account_id, step_number, variant_index, send_at, status, thread_id, parent_message_id
		) VALUES (?, ?, ?, 2, ?, ?, 'pending', ?, ?)`,
		campaignID, leadID, accountID, variantIndex, sendAt.UTC().Format(time.RFC3339), threadID, parentMessageID)
	if err != nil {
		t.Fatalf("inserting pending follow-up send: %v", err)
	}
}

func decodeRawMessage(t *testing.T, raw string) string {
	t.Helper()
	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decoding raw message: %v", err)
	}
	return string(decoded)
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

func TestTick_SendNowIgnoresSchedule(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Schedule a send far in the future
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(72*time.Hour))

	mock := &MockGWS{}

	// Without SendNow, should NOT send
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.Sent != 0 {
		t.Errorf("expected 0 sent without SendNow, got %d", result.Sent)
	}

	// With SendNow, should send despite future send_at
	result, err = Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true, SendNow: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.Sent != 1 {
		t.Errorf("expected 1 sent with SendNow, got %d", result.Sent)
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
	if result.Skipped != 0 {
		t.Errorf("expected 0 skipped after proactive rebalance, got %d", result.Skipped)
	}

	var secondStatus, secondSendAt string
	db.QueryRow("SELECT status, send_at FROM scheduled_sends WHERE lead_id = ? AND campaign_id = ?", leadIDs[1], campaignID).
		Scan(&secondStatus, &secondSendAt)
	if secondStatus != "pending" {
		t.Fatalf("expected second send to remain pending, got %q", secondStatus)
	}
	movedAt, err := parseDBTimestamp(secondSendAt)
	if err != nil {
		t.Fatalf("parsing rescheduled send_at: %v", err)
	}
	if !movedAt.After(now) {
		t.Fatalf("expected second send to be deferred after %s, got %s", now.Format(time.RFC3339), secondSendAt)
	}
}

func TestTick_Step1BackfillsThreadID(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	// Step 1 due now, step 2 in the future
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 2, now.Add(72*time.Hour))

	mock := &MockGWS{
		SendMsgID:        "gmail-msg-abc",
		SendThreadID:     "thread-xyz",
		SendRFCMessageID: "<sent-1@example.com>",
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
	if parentMsgID != "<sent-1@example.com>" {
		t.Errorf("expected parent_message_id '<sent-1@example.com>', got %q", parentMsgID)
	}

	var step1MessageID string
	db.QueryRow(`SELECT message_id FROM scheduled_sends
		WHERE campaign_id = ? AND lead_id = ? AND step_number = 1`,
		campaignID, leadIDs[0]).Scan(&step1MessageID)
	if step1MessageID != "<sent-1@example.com>" {
		t.Errorf("expected step 1 message_id '<sent-1@example.com>', got %q", step1MessageID)
	}
}

func TestTick_RebalancesFollowUpsFromActualSentTime(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, time.April, 8, 9, 0, 0, 0, time.UTC)

	seqYAML := `name: Test
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Step 1"
  - step: 2
    delay: 4
    body: "Step 2"
  - step: 3
    delay: 7
    body: "Step 3"
`

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 10)")
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end, send_days, timezone)
		VALUES ('delayed', 'active', 'seq.yml', ?, '09:00', '17:00', '0,1,2,3,4,5,6', 'UTC')`, seqYAML)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('alice@x.com', 'Alice', 'Acme', 'x.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-24*time.Hour))
	insertPendingSend(t, db, 1, 1, 1, 2, now.Add(3*24*time.Hour))
	insertPendingSend(t, db, 1, 1, 1, 3, now.Add(10*24*time.Hour))

	mock := &MockGWS{
		SendMsgID:        "gmail-msg-1",
		SendThreadID:     "thread-1",
		SendRFCMessageID: "<sent-1@example.com>",
	}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", result.Sent)
	}

	var step2SendAt, step3SendAt, threadID, parentMessageID string
	db.QueryRow("SELECT send_at, thread_id, parent_message_id FROM scheduled_sends WHERE campaign_id = 1 AND lead_id = 1 AND step_number = 2").
		Scan(&step2SendAt, &threadID, &parentMessageID)
	db.QueryRow("SELECT send_at FROM scheduled_sends WHERE campaign_id = 1 AND lead_id = 1 AND step_number = 3").
		Scan(&step3SendAt)

	if step2SendAt != "2026-04-12T09:00:00Z" {
		t.Fatalf("expected step 2 to move to 2026-04-12T09:00:00Z, got %q", step2SendAt)
	}
	if step3SendAt != "2026-04-19T09:00:00Z" {
		t.Fatalf("expected step 3 to chain from the moved step 2, got %q", step3SendAt)
	}
	if threadID != "thread-1" {
		t.Fatalf("expected thread_id 'thread-1', got %q", threadID)
	}
	if parentMessageID != "<sent-1@example.com>" {
		t.Fatalf("expected parent_message_id '<sent-1@example.com>', got %q", parentMessageID)
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
	replies, _, err := ProcessReplies(db, mock, accounts)
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

	// Outside window: send stays pending (not counted as skipped or failed)
	if result.Sent != 0 {
		t.Errorf("expected 0 sent, got %d", result.Sent)
	}
	if result.Skipped != 0 {
		t.Errorf("expected 0 skipped (window skip leaves pending), got %d", result.Skipped)
	}

	// Verify send is still pending in DB
	var status string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&status)
	if status != "pending" {
		t.Errorf("expected status 'pending' (outside window), got %q", status)
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

func TestTick_SendDayEnforcement(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)

	// Set send days to weekdays only (1-5 = Mon-Fri)
	db.Exec("UPDATE campaigns SET send_days = '1,2,3,4,5' WHERE id = ?", campaignID)

	// Set current time to a Saturday at noon UTC
	// 2025-01-04 is a Saturday
	now := time.Date(2025, 1, 4, 12, 0, 0, 0, time.UTC)
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	// Outside send days: send stays pending (not counted as skipped or failed)
	if result.Sent != 0 {
		t.Errorf("expected 0 sent, got %d", result.Sent)
	}
	if result.Skipped != 0 {
		t.Errorf("expected 0 skipped (day skip leaves pending), got %d", result.Skipped)
	}

	// Verify send is still pending in DB
	var status string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&status)
	if status != "pending" {
		t.Errorf("expected status 'pending' (Saturday), got %q", status)
	}
}

func TestTick_DailyLimitTimezone(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)

	// Use US Eastern (UTC-5) timezone
	eastern, _ := time.LoadLocation("America/New_York")

	// Set current time to 11pm Eastern = 4am next day UTC
	// 2025-01-06 Monday 23:00 Eastern = 2025-01-07 04:00 UTC
	now := time.Date(2025, 1, 7, 4, 0, 0, 0, time.UTC)

	// Pre-populate 49 sent events "today" (Eastern Monday) at 10am Eastern = 3pm UTC
	todayMorning := time.Date(2025, 1, 6, 15, 0, 0, 0, time.UTC) // 10am Eastern
	for i := 0; i < 49; i++ {
		db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
			VALUES (?, ?, ?, 'sent', 1, ?, ?, ?)`,
			campaignID, leadIDs[0], accountIDs[0],
			fmt.Sprintf("msg-pre-%d", i), fmt.Sprintf("thread-pre-%d", i),
			todayMorning.Format(time.RFC3339))
	}

	// Insert a pending send due now
	insertPendingSend(t, db, campaignID, leadIDs[1], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	// With Eastern timezone, all 49 events are "today" (Monday Eastern)
	// Daily limit is 50, so 1 more send should be allowed
	result, err := Tick(TickConfig{
		DB:       db,
		GWS:      mock,
		Now:      now,
		NoSleep:  true,
		Timezone: eastern,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent (49/50 daily limit in Eastern tz), got %d", result.Sent)
	}

	// Now with UTC timezone, the 49 events are split across two UTC days
	// (most from yesterday UTC, some from today UTC), so limit appears lower
	// This proves the timezone matters for correct counting
}

func TestTick_CustomFieldsRendered(t *testing.T) {
	db := testDB(t)

	// Create account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Create a sequence that uses a custom field
	seqYAML := `name: Custom Fields Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}, {{title}} at {{company}}"
    body: "Hello {{first_name}}, I see you're the {{title}} at {{company}}"
`
	// Create campaign with sequence_content stored in DB
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('test-custom', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)

	// Create lead with custom_fields JSON
	db.Exec(`INSERT INTO leads (email, first_name, company, domain, custom_fields)
		VALUES ('alice@corp.com', 'Alice', 'Corp Inc', 'corp.com', '{"title":"VP of Sales"}')`)

	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent, got %d", result.Sent)
	}

	// Verify custom field was available during rendering by checking loadLeadFields
	fields, err := loadLeadFields(db, 1)
	if err != nil {
		t.Fatalf("loadLeadFields error: %v", err)
	}
	if fields["title"] != "VP of Sales" {
		t.Errorf("expected custom field 'title' = 'VP of Sales', got %q", fields["title"])
	}
}

func TestTick_SequenceFromDBContent(t *testing.T) {
	db := testDB(t)

	// Create account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Store sequence content directly in DB (no file needed)
	seqYAML := `name: DB Stored Sequence
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hello {{first_name}}"
    body: "Hi {{first_name}} at {{company}}"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('db-seq-test', 'active', '/nonexistent/path.yml', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)

	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	// Should succeed even though sequence_file points to nonexistent path
	// because sequence_content is populated
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent (sequence from DB content), got %d", result.Sent)
	}
	if result.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", result.Failed)
	}
}

func TestTick_SequenceFallbackToFile(t *testing.T) {
	db := testDB(t)

	// Create account
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Campaign with empty sequence_content (simulating pre-migration campaign)
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('file-fallback', 'active', 'testdata/seq.yml', '', '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`)

	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}

	// Should fall back to reading from file path
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Errorf("expected 1 sent (sequence from file fallback), got %d", result.Sent)
	}
}

func TestLoadLeadFields_CustomFields(t *testing.T) {
	db := testDB(t)

	// Lead with custom fields
	db.Exec(`INSERT INTO leads (email, first_name, last_name, company, domain, custom_fields)
		VALUES ('test@example.com', 'Test', 'User', 'Example', 'example.com', '{"title":"CTO","city":"NYC"}')`)

	fields, err := loadLeadFields(db, 1)
	if err != nil {
		t.Fatalf("loadLeadFields error: %v", err)
	}

	if fields["title"] != "CTO" {
		t.Errorf("expected title=CTO, got %q", fields["title"])
	}
	if fields["city"] != "NYC" {
		t.Errorf("expected city=NYC, got %q", fields["city"])
	}
	// Built-in fields should still be present
	if fields["email"] != "test@example.com" {
		t.Errorf("expected email=test@example.com, got %q", fields["email"])
	}
}

func TestLoadLeadFields_CustomFieldsDoNotOverrideBuiltins(t *testing.T) {
	db := testDB(t)

	// Custom fields trying to override built-in 'email'
	db.Exec(`INSERT INTO leads (email, first_name, last_name, company, domain, custom_fields)
		VALUES ('real@example.com', 'Test', '', '', '', '{"email":"fake@evil.com"}')`)

	fields, err := loadLeadFields(db, 1)
	if err != nil {
		t.Fatalf("loadLeadFields error: %v", err)
	}

	// Built-in should win over custom_fields
	if fields["email"] != "real@example.com" {
		t.Errorf("expected email=real@example.com (built-in), got %q", fields["email"])
	}
}

func TestLoadLeadFields_EmptyCustomFields(t *testing.T) {
	db := testDB(t)

	db.Exec(`INSERT INTO leads (email, first_name, last_name, company, domain, custom_fields)
		VALUES ('test@example.com', 'Test', '', '', '', '{}')`)

	fields, err := loadLeadFields(db, 1)
	if err != nil {
		t.Fatalf("loadLeadFields error: %v", err)
	}

	if fields["email"] != "test@example.com" {
		t.Errorf("expected email=test@example.com, got %q", fields["email"])
	}
}

func TestPreloadDailyCounts_Timezone(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('test', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")

	eastern, _ := time.LoadLocation("America/New_York")

	// Insert an event at 11pm Eastern Monday = 4am UTC Tuesday
	// 2025-01-06 is a Monday
	eventTime := time.Date(2025, 1, 7, 4, 0, 0, 0, time.UTC) // 11pm Eastern Monday
	_, err := db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp)
		VALUES (1, 1, 1, 'sent', 1, ?)`, eventTime.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("inserting event: %v", err)
	}

	// Count with Eastern timezone — event should be "today" (Monday Eastern)
	now := time.Date(2025, 1, 7, 4, 30, 0, 0, time.UTC) // 11:30pm Eastern Monday
	counts, err := preloadDailyCounts(db, now, eastern)
	if err != nil {
		t.Fatalf("preloadDailyCounts error: %v", err)
	}

	if counts[1] != 1 {
		t.Errorf("expected 1 event counted for today (Eastern), got %d", counts[1])
	}

	// Count with UTC — event is "today" in UTC (Tuesday), which is correct
	// but the "day" starts at midnight UTC, not midnight Eastern
	countsUTC, err := preloadDailyCounts(db, now, time.UTC)
	if err != nil {
		t.Fatalf("preloadDailyCounts error: %v", err)
	}

	// In UTC, the event at 4am Tuesday is counted for Tuesday
	// now is 4:30am Tuesday UTC, so start of today UTC is midnight Tuesday
	// The event at 4am Tuesday is after midnight Tuesday UTC, so it counts
	if countsUTC[1] != 1 {
		t.Errorf("expected 1 event counted for today (UTC), got %d", countsUTC[1])
	}
}

func TestIsInSendWindow_ChecksDay(t *testing.T) {
	db := testDB(t)

	// Campaign with weekday-only send days and 09:00-17:00 window
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end,
		send_days, timezone) VALUES ('test', 'active', 'testdata/seq.yml', '09:00', '17:00', '1,2,3,4,5', 'UTC')`)

	// Saturday at noon — should be outside send days
	saturday := time.Date(2025, 1, 4, 12, 0, 0, 0, time.UTC)
	if isInSendWindow(db, 1, saturday) {
		t.Error("expected isInSendWindow=false on Saturday with weekday-only send_days")
	}

	// Monday at noon — should be in window
	monday := time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC)
	if !isInSendWindow(db, 1, monday) {
		t.Error("expected isInSendWindow=true on Monday at noon")
	}

	// Monday at 8am — in send day but before window
	mondayEarly := time.Date(2025, 1, 6, 8, 0, 0, 0, time.UTC)
	if isInSendWindow(db, 1, mondayEarly) {
		t.Error("expected isInSendWindow=false on Monday at 8am (before 09:00 window)")
	}
}

func TestTick_EmptySubjectAbortsSend(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Sequence where subject is ONLY a placeholder
	seqYAML := `name: Empty Subject Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "{{subject_line}}"
    body: "Hello there"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('empty-subj', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)

	// Lead has no subject_line field → placeholder gets stripped → empty subject
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 0 {
		t.Errorf("expected 0 sent (empty subject should abort), got %d", result.Sent)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
	if len(mock.SentEmails) != 0 {
		t.Error("email should NOT have been sent with empty subject")
	}

	var status string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE id = 1").Scan(&status)
	if status != "failed" {
		t.Errorf("expected status 'failed', got %q", status)
	}
}

func TestTick_FollowUpEmptySubjectUsesOriginalSubject(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	seqYAML := `name: Follow Up Empty Subject
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Quick question about Acme"
    body: "Hello there"
  - step: 2
    delay: 3
    subject: ""
    body: "Following up..."
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('followup-empty-subject', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Acme', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingFollowUpSend(t, db, 1, 1, 1, 0, now.Add(-1*time.Hour), "thread-123", "<msg-1@example.com>")

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", result.Sent)
	}
	if len(mock.SentEmails) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(mock.SentEmails))
	}

	msg := decodeRawMessage(t, mock.SentEmails[0].RawMsg)
	if !strings.Contains(msg, "Subject: Re: Quick question about Acme") {
		t.Errorf("expected follow-up subject to use rendered step-1 subject, got:\n%s", msg)
	}
	if mock.SentEmails[0].ThreadID != "thread-123" {
		t.Errorf("expected send threadID 'thread-123', got %q", mock.SentEmails[0].ThreadID)
	}
	if !strings.Contains(msg, "In-Reply-To: <msg-1@example.com>") {
		t.Errorf("expected In-Reply-To header, got:\n%s", msg)
	}
	if !strings.Contains(msg, "References: <msg-1@example.com>") {
		t.Errorf("expected References header, got:\n%s", msg)
	}
}

func TestTick_FollowUpUsesRenderedPlaceholderSubject(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	seqYAML := `name: Follow Up Placeholder Subject
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Leads for {{first_name}}"
    body: "Hello there"
  - step: 2
    delay: 3
    subject: ""
    body: "Following up..."
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('followup-placeholder-subject', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('john@acme.com', 'John', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingFollowUpSend(t, db, 1, 1, 1, 0, now.Add(-1*time.Hour), "thread-123", "<msg-1@example.com>")

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", result.Sent)
	}

	msg := decodeRawMessage(t, mock.SentEmails[0].RawMsg)
	if !strings.Contains(msg, "Subject: Re: Leads for John") {
		t.Errorf("expected rendered placeholder subject, got:\n%s", msg)
	}
}

func TestTick_FollowUpUsesRenderedVariantSubject(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	seqYAML := `name: Follow Up Variant Subject
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Default subject"
    body: "Hello there"
    variants:
      - subject: "Offer for {{company}}"
        body: "Variant body"
  - step: 2
    delay: 3
    subject: ""
    body: "Following up..."
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('followup-variant-subject', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('john@acme.com', 'John', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingFollowUpSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour), "thread-123", "<msg-1@example.com>")

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", result.Sent)
	}

	msg := decodeRawMessage(t, mock.SentEmails[0].RawMsg)
	if !strings.Contains(msg, "Subject: Re: Offer for Acme") {
		t.Errorf("expected rendered variant subject, got:\n%s", msg)
	}
}

func TestTick_EmptyBodyAbortsSend(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Sequence where body is ONLY a placeholder
	seqYAML := `name: Empty Body Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hello"
    body: "{{custom_body}}"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('empty-body', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)

	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}

	if result.Sent != 0 {
		t.Errorf("expected 0 sent (empty body should abort), got %d", result.Sent)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
}

func TestTick_FailedSendRecordsErrorAndEvent(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{SendError: fmt.Errorf("gws: token expired")}

	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}

	// Verify error_message is stored on scheduled_sends
	var errorMsg string
	db.QueryRow("SELECT error_message FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&errorMsg)
	if errorMsg != "gws: token expired" {
		t.Errorf("expected error_message %q, got %q", "gws: token expired", errorMsg)
	}

	// Verify a 'failed' event was inserted
	var eventCount int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE campaign_id = ? AND type = 'failed'", campaignID).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("expected 1 failed event, got %d", eventCount)
	}

	// Verify the event metadata contains the error message
	var metadata string
	db.QueryRow("SELECT metadata FROM events WHERE campaign_id = ? AND type = 'failed'", campaignID).Scan(&metadata)
	if metadata != "gws: token expired" {
		t.Errorf("expected event metadata %q, got %q", "gws: token expired", metadata)
	}
}

func TestTick_EmptySubjectRecordsError(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")

	// Sequence where subject is ONLY a placeholder that won't resolve
	seqYAML := `name: Empty Subject Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "{{missing_var}}"
    body: "Hello there"
`
	db.Exec(`INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone) VALUES ('empty-subj', 'active', 'N/A', ?, '00:00', '23:59', '0,1,2,3,4,5,6', 'UTC')`,
		seqYAML)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('test@example.com', 'Test', 'Example', 'example.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	now := time.Now().UTC()
	insertPendingSend(t, db, 1, 1, 1, 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}
	result, err := Tick(TickConfig{DB: db, GWS: mock, Now: now, NoSleep: true})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}

	// Verify error_message is stored
	var errorMsg string
	db.QueryRow("SELECT error_message FROM scheduled_sends WHERE id = 1").Scan(&errorMsg)
	if errorMsg != "empty subject after rendering" {
		t.Errorf("expected error_message %q, got %q", "empty subject after rendering", errorMsg)
	}

	// Verify failed event exists
	var eventCount int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'failed'").Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("expected 1 failed event, got %d", eventCount)
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
