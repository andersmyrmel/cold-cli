package internal

import (
	"database/sql"
	"testing"
	"time"
)

func TestProcessReplies_Dedup(t *testing.T) {
	db := setupReplyTestDB(t)

	// Insert sent event
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-1')`)

	// Insert pending step 2
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "reply-1", ThreadID: "thread-1", InReplyTo: "<sent-1@gmail.com>", From: "john@acme.com"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}

	// First call: detects 1 reply
	replies1, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("first ProcessReplies error: %v", err)
	}
	if replies1 != 1 {
		t.Errorf("expected 1 reply first time, got %d", replies1)
	}

	// Second call with same messages: should detect 0 (deduped)
	replies2, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("second ProcessReplies error: %v", err)
	}
	if replies2 != 0 {
		t.Errorf("expected 0 replies second time (dedup), got %d", replies2)
	}

	// Should have exactly 1 reply event, not 2
	var count int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'reply'").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 reply event total, got %d", count)
	}
}

func TestProcessBounces_Dedup(t *testing.T) {
	db := setupReplyTestDB(t)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "bounce-1", From: "MAILER-DAEMON@google.com", Subject: "Delivery failed", Snippet: "Delivery to john@acme.com failed"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}

	// First call: detects 1 bounce
	bounces1, err := ProcessBounces(db, mock, accounts)
	if err != nil {
		t.Fatalf("first ProcessBounces error: %v", err)
	}
	if bounces1 != 1 {
		t.Errorf("expected 1 bounce first time, got %d", bounces1)
	}

	// Second call: should detect 0 (deduped)
	bounces2, err := ProcessBounces(db, mock, accounts)
	if err != nil {
		t.Fatalf("second ProcessBounces error: %v", err)
	}
	if bounces2 != 0 {
		t.Errorf("expected 0 bounces second time (dedup), got %d", bounces2)
	}
}

func TestProcessBounces_FiltersDaemonAddress(t *testing.T) {
	// Should not extract "mailer-daemon@google.com" as the bounced email
	email := extractBouncedEmail("mailer-daemon@google.com says: delivery to john@acme.com failed", "")
	if email != "john@acme.com" {
		t.Errorf("expected john@acme.com, got %q", email)
	}

	// Should not match postmaster as a bounced email
	email = extractBouncedEmail("postmaster@outlook.com reports failure for jane@foo.com", "")
	if email != "jane@foo.com" {
		t.Errorf("expected jane@foo.com, got %q", email)
	}
}

func TestProcessBounces_VariousNDRFormats(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
		subject string
		want    string
	}{
		{"gmail NDR", "Delivery to john@acme.com has failed permanently", "Delivery Status Notification", "john@acme.com"},
		{"outlook NDR", "Undeliverable: Your message to jane@foo.com couldn't be delivered", "", "jane@foo.com"},
		{"angle brackets", "The email to <bob@bar.org> was rejected", "", "bob@bar.org"},
		{"quoted", `Message to "alice@test.com" bounced`, "", "alice@test.com"},
		{"no email", "Delivery has failed for unknown reasons", "", ""},
		{"only daemon address", "mailer-daemon@google.com", "", ""},
		{"only postmaster", "postmaster@outlook.com", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBouncedEmail(tt.snippet, tt.subject)
			if got != tt.want {
				t.Errorf("extractBouncedEmail(%q, %q) = %q, want %q", tt.snippet, tt.subject, got, tt.want)
			}
		})
	}
}

func TestGetSetLastPollAt(t *testing.T) {
	db := testDB(t)

	// No value set — should return ~24h ago
	lastPoll := GetLastPollAt(db)
	if time.Since(lastPoll) < 23*time.Hour {
		t.Errorf("expected default ~24h ago, got %v ago", time.Since(lastPoll))
	}

	// Set and read back
	now := time.Now().UTC().Truncate(time.Second)
	SetLastPollAt(db, now)

	got := GetLastPollAt(db)
	if !got.Equal(now) {
		t.Errorf("expected %v, got %v", now, got)
	}

	// Update
	later := now.Add(1 * time.Hour)
	SetLastPollAt(db, later)
	got = GetLastPollAt(db)
	if !got.Equal(later) {
		t.Errorf("expected %v, got %v", later, got)
	}
}

func TestProcessReplies_LeadInMultipleCampaigns(t *testing.T) {
	db := testDB(t)

	// Setup: 1 account, 2 campaigns, 1 lead in both
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('camp1', 'active', 'seq.yml')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('camp2', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (2, 1, 'active')")

	// Sent from campaign 1
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-camp1@gmail.com>', 'thread-1')`)

	// Pending sends in both campaigns
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (2, 1, 1, 1, '2099-01-01', 'pending')`)

	// Reply to campaign 1's email
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "reply-1", ThreadID: "thread-1", InReplyTo: "<sent-camp1@gmail.com>", From: "john@acme.com"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	ProcessReplies(db, mock, accounts)

	// Campaign 1's sends should be skipped
	var s1 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 1").Scan(&s1)
	if s1 != "skipped" {
		t.Errorf("campaign 1 send should be 'skipped', got %q", s1)
	}

	// Campaign 2's sends should still be pending (reply was to campaign 1)
	var s2 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 2").Scan(&s2)
	if s2 != "pending" {
		t.Errorf("campaign 2 send should still be 'pending', got %q", s2)
	}
}

// setupReplyTestDB creates minimal test data for reply/bounce tests.
func setupReplyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('test', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	return db
}
