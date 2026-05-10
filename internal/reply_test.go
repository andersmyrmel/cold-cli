package internal

import (
	"database/sql"
	"strings"
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
	replies1, _, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("first ProcessReplies error: %v", err)
	}
	if replies1 != 1 {
		t.Errorf("expected 1 reply first time, got %d", replies1)
	}

	// Second call with same messages: should detect 0 (deduped)
	replies2, _, err := ProcessReplies(db, mock, accounts)
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

func TestProcessReplies_PersistsInboundEmailMessageSnapshot(t *testing.T) {
	db := setupReplyTestDB(t)

	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-1')`)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:        "reply-1",
				ThreadID:  "thread-1",
				InReplyTo: "<sent-1@gmail.com>",
				From:      "John Acme <john@acme.com>",
				To:        "sender@x.com",
				Subject:   "Re: Hi John",
				Snippet:   "Thanks for reaching out.",
				TextBody:  "Thanks for reaching out.\nCan you send pricing?\n\nOn Tue, May 5, 2026 at 9:14 AM Sender <sender@x.com> wrote:\n> Hi John",
				Headers: map[string]string{
					"Message-ID":   "<reply-1@gmail.com>",
					"In-Reply-To":  "<sent-1@gmail.com>",
					"Content-Type": "text/plain",
				},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, unsubs, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 1 || unsubs != 0 {
		t.Fatalf("expected 1 reply and 0 unsubscribes, got replies=%d unsubs=%d", replies, unsubs)
	}

	var msg EmailMessage
	if err := db.QueryRow(`
		SELECT campaign_id, lead_id, account_id, direction, type, step_number,
			message_id, thread_id, in_reply_to, from_email, to_emails, subject,
			text_body, display_body, snippet, raw_headers
		FROM email_messages
		WHERE message_id = ?`,
		"reply-1",
	).Scan(
		&msg.CampaignID,
		&msg.LeadID,
		&msg.AccountID,
		&msg.Direction,
		&msg.Type,
		&msg.StepNumber,
		&msg.MessageID,
		&msg.ThreadID,
		&msg.InReplyTo,
		&msg.FromEmail,
		&msg.ToEmails,
		&msg.Subject,
		&msg.TextBody,
		&msg.DisplayBody,
		&msg.Snippet,
		&msg.RawHeaders,
	); err != nil {
		t.Fatalf("loading inbound email message snapshot: %v", err)
	}

	if msg.CampaignID != 1 || msg.LeadID != 1 || msg.AccountID != 1 {
		t.Fatalf("snapshot belongs to campaign=%d lead=%d account=%d", msg.CampaignID, msg.LeadID, msg.AccountID)
	}
	if msg.Direction != EmailMessageDirectionInbound {
		t.Errorf("expected inbound direction, got %q", msg.Direction)
	}
	if msg.Type != EmailMessageTypeReply {
		t.Errorf("expected reply type, got %q", msg.Type)
	}
	if msg.StepNumber != 0 {
		t.Errorf("expected step 0, got %d", msg.StepNumber)
	}
	if msg.ThreadID != "thread-1" {
		t.Errorf("expected thread-1, got %q", msg.ThreadID)
	}
	if msg.InReplyTo != "<sent-1@gmail.com>" {
		t.Errorf("expected In-Reply-To snapshot, got %q", msg.InReplyTo)
	}
	if msg.FromEmail != "John Acme <john@acme.com>" {
		t.Errorf("expected from snapshot, got %q", msg.FromEmail)
	}
	if msg.ToEmails != "sender@x.com" {
		t.Errorf("expected to snapshot, got %q", msg.ToEmails)
	}
	if msg.Subject != "Re: Hi John" {
		t.Errorf("expected subject snapshot, got %q", msg.Subject)
	}
	if msg.TextBody != "Thanks for reaching out.\nCan you send pricing?\n\nOn Tue, May 5, 2026 at 9:14 AM Sender <sender@x.com> wrote:\n> Hi John" {
		t.Errorf("expected text body snapshot, got %q", msg.TextBody)
	}
	if msg.DisplayBody != "Thanks for reaching out.\nCan you send pricing?" {
		t.Errorf("expected display body snapshot, got %q", msg.DisplayBody)
	}
	if msg.Snippet != "Thanks for reaching out." {
		t.Errorf("expected snippet snapshot, got %q", msg.Snippet)
	}
	if !strings.Contains(msg.RawHeaders, `"Message-ID":"<reply-1@gmail.com>"`) {
		t.Errorf("expected raw headers JSON to include Message-ID, got %q", msg.RawHeaders)
	}
}

func TestProcessReplies_StoresRepliesOnOriginalSendingAccount(t *testing.T) {
	db := setupReplyTestDB(t)

	db.Exec(`INSERT INTO accounts (email) VALUES ('alias@x.com')`)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-1')`)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "reply-1",
				ThreadID: "thread-1",
				From:     "John Acme <john@acme.com>",
				To:       "alias@x.com",
				Subject:  "Re: Hi John",
				Snippet:  "Thanks",
				TextBody: "Thanks",
			},
		},
	}

	replies, _, err := ProcessReplies(db, mock, []Account{{ID: 2, Email: "alias@x.com", DailyLimit: 50, Status: "active"}})
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 1 {
		t.Fatalf("expected 1 reply, got %d", replies)
	}

	var eventAccountID, messageAccountID int64
	if err := db.QueryRow("SELECT account_id FROM events WHERE message_id = 'reply-1'").Scan(&eventAccountID); err != nil {
		t.Fatalf("loading reply event: %v", err)
	}
	if err := db.QueryRow("SELECT account_id FROM email_messages WHERE message_id = 'reply-1'").Scan(&messageAccountID); err != nil {
		t.Fatalf("loading reply email message: %v", err)
	}
	if eventAccountID != 1 || messageAccountID != 1 {
		t.Fatalf("expected original sending account 1, got event=%d message=%d", eventAccountID, messageAccountID)
	}
}

func TestProcessIMAPReplies(t *testing.T) {
	db := setupReplyTestDB(t)
	db.Exec(`UPDATE accounts SET provider = ? WHERE id = 1`, AccountProviderSMTPIMAP)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@example.com>', '<sent-1@example.com>')`)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	imapMock := &MockIMAPMessageLister{
		Messages: []GWSMessage{
			{ID: "<reply-1@example.com>", InReplyTo: "<sent-1@example.com>", From: "john@acme.com", Subject: "Re: Hello"},
		},
	}
	accounts := []Account{{ID: 1, Email: "sender@x.com", Provider: AccountProviderSMTPIMAP, Status: "active"}}

	replies, unsubs, err := ProcessIMAPReplies(db, imapMock, accounts)
	if err != nil {
		t.Fatalf("ProcessIMAPReplies error: %v", err)
	}
	if replies != 1 || unsubs != 0 {
		t.Fatalf("expected 1 reply and 0 unsubscribes, got replies=%d unsubs=%d", replies, unsubs)
	}
	if len(imapMock.ListCalls) != 1 {
		t.Fatalf("expected 1 IMAP list call, got %d", len(imapMock.ListCalls))
	}
	if imapMock.ListCalls[0].IncludeSpamTrash {
		t.Fatal("reply polling should not include spam/trash")
	}

	var status string
	db.QueryRow("SELECT status FROM campaign_leads WHERE campaign_id = 1 AND lead_id = 1").Scan(&status)
	if status != "replied" {
		t.Errorf("expected lead campaign status replied, got %s", status)
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

func TestProcessIMAPBounces(t *testing.T) {
	db := setupReplyTestDB(t)
	db.Exec(`UPDATE accounts SET provider = ? WHERE id = 1`, AccountProviderSMTPIMAP)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 1, '2099-01-01', 'pending')`)

	imapMock := &MockIMAPMessageLister{
		Messages: []GWSMessage{
			{
				ID:      "<bounce-1@example.com>",
				From:    "MAILER-DAEMON@migadu.com",
				Subject: "Undelivered Mail Returned to Sender",
				Snippet: "Delivery to john@acme.com failed",
				Headers: map[string]string{},
			},
			{
				ID:      "<ordinary-1@example.com>",
				From:    "someone@example.com",
				Subject: "Normal message",
				Snippet: "john@acme.com is mentioned, but this is not a bounce",
				Headers: map[string]string{},
			},
		},
	}
	accounts := []Account{{ID: 1, Email: "sender@x.com", Provider: AccountProviderSMTPIMAP, Status: "active"}}

	bounces, err := ProcessIMAPBounces(db, imapMock, accounts)
	if err != nil {
		t.Fatalf("ProcessIMAPBounces error: %v", err)
	}
	if bounces != 1 {
		t.Fatalf("expected 1 bounce, got %d", bounces)
	}
	if len(imapMock.ListCalls) != 1 {
		t.Fatalf("expected 1 IMAP list call, got %d", len(imapMock.ListCalls))
	}
	if !imapMock.ListCalls[0].IncludeSpamTrash {
		t.Fatal("bounce polling should include spam/trash")
	}

	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&globalStatus)
	if globalStatus != "bounced" {
		t.Errorf("expected bounced lead, got %s", globalStatus)
	}
}

func TestProcessBounces_IncludesSpamTrash(t *testing.T) {
	db := setupReplyTestDB(t)

	mock := &MockGWS{}
	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}

	if _, err := ProcessBounces(db, mock, accounts); err != nil {
		t.Fatalf("ProcessBounces error: %v", err)
	}
	if len(mock.ListCalls) != 1 {
		t.Fatalf("expected 1 ListMessages call, got %d", len(mock.ListCalls))
	}
	if !mock.ListCalls[0].IncludeSpamTrash {
		t.Fatal("bounce polling should include spam/trash so NDRs in Spam are detected")
	}
}

func TestProcessReplies_DoesNotIncludeSpamTrash(t *testing.T) {
	db := setupReplyTestDB(t)

	mock := &MockGWS{}
	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}

	if _, _, err := ProcessReplies(db, mock, accounts); err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if len(mock.ListCalls) != 1 {
		t.Fatalf("expected 1 ListMessages call, got %d", len(mock.ListCalls))
	}
	if mock.ListCalls[0].IncludeSpamTrash {
		t.Fatal("reply polling should stay inbox-scoped and not include spam/trash")
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

func TestBounce_ThreadMatching(t *testing.T) {
	db := setupReplyTestDB(t)

	// We sent an email to john@acme.com in thread "thread-abc"
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-to-john@gmail.com>', 'thread-abc')`)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	// NDR arrives in the same thread — no bounced email in snippet
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "ndr-1",
				ThreadID: "thread-abc",
				From:     "MAILER-DAEMON@googlemail.com",
				Subject:  "Delivery Status Notification",
				Snippet:  "Message not delivered. You're sending this from a different address or alias.",
				Headers:  map[string]string{},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, err := ProcessBounces(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessBounces error: %v", err)
	}
	if bounces != 1 {
		t.Errorf("expected 1 bounce via thread matching, got %d", bounces)
	}

	// Lead should be bounced
	var status string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&status)
	if status != "bounced" {
		t.Errorf("expected global_status 'bounced', got %q", status)
	}

	// Pending send should be skipped
	var ssStatus string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE id = 1").Scan(&ssStatus)
	if ssStatus != "skipped" {
		t.Errorf("expected scheduled_send 'skipped', got %q", ssStatus)
	}
}

func TestBounce_XFailedRecipientsHeader(t *testing.T) {
	db := setupReplyTestDB(t)

	// NDR with X-Failed-Recipients header, no thread match
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "ndr-2",
				ThreadID: "some-other-thread",
				From:     "MAILER-DAEMON@migadu.com",
				Subject:  "Undelivered Mail Returned to Sender",
				Snippet:  "This is the mail system at host mta0.migadu.com. I'm sorry to have to inform you...",
				Headers: map[string]string{
					"X-Failed-Recipients": "john@acme.com",
				},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, _ := ProcessBounces(db, mock, accounts)
	if bounces != 1 {
		t.Errorf("expected 1 bounce via X-Failed-Recipients, got %d", bounces)
	}

	var status string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&status)
	if status != "bounced" {
		t.Errorf("expected 'bounced', got %q", status)
	}
}

func TestBounce_SnippetFallback(t *testing.T) {
	db := setupReplyTestDB(t)

	// NDR with no thread match, no X-Failed-Recipients, but email in snippet
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "ndr-3",
				ThreadID: "unrelated-thread",
				From:     "mailer-daemon@googlemail.com",
				Subject:  "Address not found",
				Snippet:  "Your message wasn't delivered to john@acme.com because the address couldn't be found.",
				Headers:  map[string]string{},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, _ := ProcessBounces(db, mock, accounts)
	if bounces != 1 {
		t.Errorf("expected 1 bounce via snippet fallback, got %d", bounces)
	}
}

func TestBounce_NoMatchSkipped(t *testing.T) {
	db := setupReplyTestDB(t)

	// NDR that doesn't match any strategy
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "ndr-4",
				ThreadID: "unknown-thread",
				From:     "MAILER-DAEMON@google.com",
				Subject:  "Delivery Status Notification",
				Snippet:  "An error occurred. Please contact support.",
				Headers:  map[string]string{},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, _ := ProcessBounces(db, mock, accounts)
	if bounces != 0 {
		t.Errorf("expected 0 bounces (no match), got %d", bounces)
	}

	// Lead should still be active
	var status string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&status)
	if status != "active" {
		t.Errorf("expected 'active', got %q", status)
	}
}

func TestBounce_RealWorldExamples(t *testing.T) {
	// Simulate the three real NDR examples from the user
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('camp1', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('paul@shopifreaks.com', 'Paul', 'shopifreaks.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('jill@modernretail.co', 'Jill', 'modernretail.co')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('info@nygolfcenter.com', 'Golf Center', 'nygolfcenter.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 2, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 3, 'active')")

	// Sent events with thread IDs
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<to-paul>', 'thread-paul')`)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 2, 1, 'sent', 1, '<to-jill>', 'thread-jill')`)
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 3, 1, 'sent', 1, '<to-golf>', 'thread-golf')`)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			// Example 1: ProductLair outgoing limit — same thread, no email in snippet
			{
				ID: "ndr-paul", ThreadID: "thread-paul",
				From: "mailer-daemon@googlemail.com", Subject: "Delivery Status Notification",
				Snippet: "Message not delivered. You're sending this from a different address or alias using the 'Send mail as' feature.",
				Headers: map[string]string{},
			},
			// Example 2: Modern Retail address not found — same thread + email in snippet
			{
				ID: "ndr-jill", ThreadID: "thread-jill",
				From: "mailer-daemon@googlemail.com", Subject: "Address not found",
				Snippet: "Your message wasn't delivered to jill@modernretail.co because the address couldn't be found",
				Headers: map[string]string{},
			},
			// Example 3: Migadu bounce — different thread, email buried in body (not in snippet)
			{
				ID: "ndr-golf", ThreadID: "thread-golf",
				From: "MAILER-DAEMON@migadu.com", Subject: "Undelivered Mail Returned to Sender",
				Snippet: "This is the mail system at host mta0.migadu.com. I'm sorry to have to inform you that your message could not be delivered",
				Headers: map[string]string{},
			},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	bounces, err := ProcessBounces(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessBounces error: %v", err)
	}

	// All 3 should be caught via thread matching
	if bounces != 3 {
		t.Errorf("expected 3 bounces from real-world examples, got %d", bounces)
	}

	// Verify all leads are bounced
	for _, email := range []string{"paul@shopifreaks.com", "jill@modernretail.co", "info@nygolfcenter.com"} {
		var status string
		db.QueryRow("SELECT global_status FROM leads WHERE email = ?", email).Scan(&status)
		if status != "bounced" {
			t.Errorf("lead %s: expected 'bounced', got %q", email, status)
		}
	}
}

func TestIsUnsubscribeRequest(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		snippet string
		want    bool
	}{
		{"exact subject", "Unsubscribe", "", true},
		{"case insensitive", "UNSUBSCRIBE", "", true},
		{"in snippet", "Re: Your email", "please unsubscribe me from this list", true},
		{"remove me", "", "Please remove me from your mailing list", true},
		{"opt out", "I want to opt out", "", true},
		{"opt-out hyphen", "opt-out please", "", true},
		{"stop emailing", "", "stop emailing me", true},
		{"do not contact", "", "do not contact me again", true},
		{"normal reply", "Re: Quick question", "Thanks for reaching out, let's chat", false},
		{"interested reply", "Re: Partnership", "This sounds interesting", false},
		{"empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsUnsubscribeRequest(tt.subject, tt.snippet)
			if got != tt.want {
				t.Errorf("IsUnsubscribeRequest(%q, %q) = %v, want %v",
					tt.subject, tt.snippet, got, tt.want)
			}
		})
	}
}

func TestProcessReplies_UnsubscribeDetected(t *testing.T) {
	db := setupReplyTestDB(t)

	// Insert sent event
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-1')`)

	// Insert pending step 2
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "unsub-1", ThreadID: "thread-1", InReplyTo: "<sent-1@gmail.com>",
				From: "john@acme.com", Subject: "Unsubscribe"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, unsubs, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}

	if replies != 0 {
		t.Errorf("expected 0 replies (should be unsubscribe), got %d", replies)
	}
	if unsubs != 1 {
		t.Errorf("expected 1 unsubscribe, got %d", unsubs)
	}

	// Lead should be blacklisted globally
	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&globalStatus)
	if globalStatus != "blacklisted" {
		t.Errorf("expected global_status 'blacklisted', got %q", globalStatus)
	}

	// Pending sends should be cancelled (BlacklistLead uses 'cancelled')
	var ssStatus string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 1 AND lead_id = 1").Scan(&ssStatus)
	if ssStatus != "cancelled" {
		t.Errorf("expected scheduled_send 'cancelled', got %q", ssStatus)
	}

	// Should have an unsubscribe event, not a reply event
	var unsubCount, replyCount int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'unsubscribe'").Scan(&unsubCount)
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'reply'").Scan(&replyCount)
	if unsubCount != 1 {
		t.Errorf("expected 1 unsubscribe event, got %d", unsubCount)
	}
	if replyCount != 0 {
		t.Errorf("expected 0 reply events, got %d", replyCount)
	}
}

func TestProcessReplies_UnsubscribeBlacklistsGlobally(t *testing.T) {
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
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (2, 1, 1, 1, '2099-01-01', 'pending')`)

	// Unsubscribe reply to campaign 1
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "unsub-1", ThreadID: "thread-1", InReplyTo: "<sent-camp1@gmail.com>",
				From: "john@acme.com", Subject: "Please remove me"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	_, unsubs, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if unsubs != 1 {
		t.Errorf("expected 1 unsubscribe, got %d", unsubs)
	}

	// Both campaigns' pending sends should be cancelled
	var s1, s2 string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 1").Scan(&s1)
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 2").Scan(&s2)
	if s1 != "cancelled" {
		t.Errorf("campaign 1 send should be 'cancelled', got %q", s1)
	}
	if s2 != "cancelled" {
		t.Errorf("campaign 2 send should be 'cancelled' (global blacklist), got %q", s2)
	}

	// Lead should be globally blacklisted
	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&globalStatus)
	if globalStatus != "blacklisted" {
		t.Errorf("expected 'blacklisted', got %q", globalStatus)
	}
}

func TestProcessReplies_ThreadIDFallback(t *testing.T) {
	db := setupReplyTestDB(t)

	// Insert sent event with thread_id
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-abc')`)

	// Insert pending step 2
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	// Reply from a DIFFERENT address (shared inbox / forwarded), no In-Reply-To,
	// but same Gmail thread
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "reply-1", ThreadID: "thread-abc", InReplyTo: "", From: "tammy@otherdomain.com",
				Subject: "Re: Your pitch"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, _, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 1 {
		t.Errorf("expected 1 reply via thread-ID fallback, got %d", replies)
	}

	// Lead should be marked replied
	var clStatus string
	db.QueryRow("SELECT status FROM campaign_leads WHERE campaign_id = 1 AND lead_id = 1").Scan(&clStatus)
	if clStatus != "replied" {
		t.Errorf("expected campaign_lead status 'replied', got %q", clStatus)
	}

	// Pending send should be skipped
	var ssStatus string
	db.QueryRow("SELECT status FROM scheduled_sends WHERE campaign_id = 1 AND lead_id = 1").Scan(&ssStatus)
	if ssStatus != "skipped" {
		t.Errorf("expected scheduled_send 'skipped', got %q", ssStatus)
	}
}

func TestProcessReplies_ThreadIDNotMatchedWhenInReplyToWorks(t *testing.T) {
	db := setupReplyTestDB(t)

	// Insert sent event
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-abc')`)

	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	// Reply with BOTH InReplyTo and ThreadID — InReplyTo should take priority
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "reply-1", ThreadID: "thread-abc", InReplyTo: "<sent-1@gmail.com>", From: "john@acme.com"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, _, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 1 {
		t.Errorf("expected 1 reply, got %d", replies)
	}
}

func TestProcessReplies_ThreadIDNoMatchSkipped(t *testing.T) {
	db := setupReplyTestDB(t)

	// No sent events — no thread to match against

	// Message with a thread_id that doesn't match anything
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "msg-1", ThreadID: "unknown-thread", InReplyTo: "", From: "random@example.com"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, _, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 0 {
		t.Errorf("expected 0 replies for unmatched thread, got %d", replies)
	}
}

func TestProcessReplies_ThreadIDUnsubscribe(t *testing.T) {
	db := setupReplyTestDB(t)

	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@gmail.com>', 'thread-abc')`)

	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 2, '2099-01-01', 'pending')`)

	// Unsubscribe via thread-ID (no In-Reply-To)
	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{ID: "unsub-1", ThreadID: "thread-abc", InReplyTo: "",
				From: "john@acme.com", Subject: "Please remove me from your list"},
		},
	}

	accounts := []Account{{ID: 1, Email: "sender@x.com", DailyLimit: 50, Status: "active"}}
	replies, unsubs, err := ProcessReplies(db, mock, accounts)
	if err != nil {
		t.Fatalf("ProcessReplies error: %v", err)
	}
	if replies != 0 {
		t.Errorf("expected 0 replies, got %d", replies)
	}
	if unsubs != 1 {
		t.Errorf("expected 1 unsubscribe via thread-ID, got %d", unsubs)
	}

	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = 1").Scan(&globalStatus)
	if globalStatus != "blacklisted" {
		t.Errorf("expected 'blacklisted', got %q", globalStatus)
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
