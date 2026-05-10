package internal

import (
	"testing"
	"time"
)

func TestListEmailThreadMessages(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, time.May, 6, 12, 0, 0, 0, time.UTC)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('test', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('jane@acme.com', 'Jane', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 2, 'active')")

	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: 1,
		LeadID:     1,
		AccountID:  1,
		Direction:  EmailMessageDirectionInbound,
		Type:       EmailMessageTypeReply,
		MessageID:  "reply-later",
		ThreadID:   "thread-1",
		Subject:    "Re: Hi",
		TextBody:   "Second",
		OccurredAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("inserting later message: %v", err)
	}
	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: 1,
		LeadID:     1,
		AccountID:  1,
		Direction:  EmailMessageDirectionOutbound,
		Type:       EmailMessageTypeSent,
		MessageID:  "sent-first",
		ThreadID:   "thread-1",
		Subject:    "Hi",
		TextBody:   "First",
		OccurredAt: now,
	}); err != nil {
		t.Fatalf("inserting first message: %v", err)
	}
	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: 1,
		LeadID:     2,
		AccountID:  1,
		Direction:  EmailMessageDirectionInbound,
		Type:       EmailMessageTypeReply,
		MessageID:  "other-lead",
		ThreadID:   "thread-1",
		Subject:    "Other",
		TextBody:   "Wrong lead",
		OccurredAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("inserting other lead message: %v", err)
	}

	messages, err := ListEmailThreadMessages(db, ListEmailThreadMessagesOpts{
		CampaignID: 1,
		LeadID:     1,
		ThreadID:   "thread-1",
	})
	if err != nil {
		t.Fatalf("ListEmailThreadMessages error: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].MessageID != "sent-first" {
		t.Fatalf("expected oldest message first, got %s", messages[0].MessageID)
	}
	if messages[1].MessageID != "reply-later" {
		t.Fatalf("expected later reply second, got %s", messages[1].MessageID)
	}
	if messages[0].LeadID != 1 || messages[1].LeadID != 1 {
		t.Fatalf("expected only lead 1 messages, got lead ids %d and %d", messages[0].LeadID, messages[1].LeadID)
	}
	if messages[1].DisplayBody != "Second" {
		t.Fatalf("expected display body Second, got %q", messages[1].DisplayBody)
	}
}

func TestBackfillEmailMessageDisplayBodiesUsesMessageType(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, time.May, 6, 12, 0, 0, 0, time.UTC)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('test', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")

	raw := "Hi John,\n\nHappy to.\n\nOn Tue, May 5, 2026 at 9:14 AM John <john@acme.com> wrote:\n> Can you send details?"
	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: 1,
		LeadID:     1,
		AccountID:  1,
		Direction:  EmailMessageDirectionOutbound,
		Type:       EmailMessageTypeManualReply,
		MessageID:  "manual-1",
		ThreadID:   "thread-1",
		TextBody:   raw,
		OccurredAt: now,
	}); err != nil {
		t.Fatalf("inserting manual reply: %v", err)
	}

	if _, err := execDB(db, "UPDATE email_messages SET display_body = text_body"); err != nil {
		t.Fatalf("reset display body: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO kv (key, value) VALUES ('email_messages.display_body_version', '3')
		ON CONFLICT(key) DO UPDATE SET value = '3'`); err != nil {
		t.Fatalf("reset display body version: %v", err)
	}
	if err := backfillEmailMessageDisplayBodies(db); err != nil {
		t.Fatalf("backfill display bodies: %v", err)
	}

	var displayBody string
	if err := db.QueryRow("SELECT display_body FROM email_messages WHERE message_id = 'manual-1'").Scan(&displayBody); err != nil {
		t.Fatalf("loading display body: %v", err)
	}
	if displayBody != "Hi John,\n\nHappy to." {
		t.Fatalf("expected stripped manual reply display body, got %q", displayBody)
	}
}

func TestSendInboxReply_SendsAndPersistsSMTPIMAPReply(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, time.May, 6, 12, 0, 0, 0, time.UTC)

	db.Exec(`INSERT INTO accounts (
		email, provider, smtp_host, smtp_port, smtp_username, smtp_password_ref, smtp_tls_mode
	) VALUES ('sender@x.com', ?, 'smtp.example.com', 587, 'sender@x.com', 'env:SMTP_PASSWORD', 'starttls')`, AccountProviderSMTPIMAP)
	db.Exec("INSERT INTO campaigns (name, status, sequence_file) VALUES ('test', 'active', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'replied')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")

	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: 1,
		LeadID:     1,
		AccountID:  1,
		Direction:  EmailMessageDirectionInbound,
		Type:       EmailMessageTypeReply,
		MessageID:  "gmail-api-reply-id",
		ThreadID:   "thread-1",
		FromEmail:  "John Acme <john@acme.com>",
		ToEmails:   "sender@x.com",
		Subject:    "Re: Hi John",
		TextBody:   "Can you send details?",
		RawHeaders: `{"Message-ID":"<reply-1@example.com>"}`,
		OccurredAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("inserting inbound message: %v", err)
	}

	smtpMock := &MockSMTPEmailSender{
		MessageID: "<manual-1@example.com>",
		ThreadID:  "thread-1",
	}
	result, err := SendInboxReply(SendInboxReplyConfig{
		DB:         db,
		CampaignID: 1,
		LeadID:     1,
		Body:       "Happy to send details.",
		Now:        now,
		SMTPSender: smtpMock,
	})
	if err != nil {
		t.Fatalf("SendInboxReply error: %v", err)
	}

	if result.MessageID != "<manual-1@example.com>" {
		t.Fatalf("expected manual message id, got %s", result.MessageID)
	}
	if result.ToEmail != "john@acme.com" {
		t.Fatalf("expected reply recipient john@acme.com, got %s", result.ToEmail)
	}
	if result.Subject != "Re: Hi John" {
		t.Fatalf("expected reply subject, got %s", result.Subject)
	}
	if len(smtpMock.SentEmails) != 1 {
		t.Fatalf("expected 1 SMTP send, got %d", len(smtpMock.SentEmails))
	}
	params := smtpMock.SentEmails[0].Params
	if params.ToEmail != "john@acme.com" {
		t.Fatalf("expected SMTP recipient john@acme.com, got %s", params.ToEmail)
	}
	if params.InReplyTo != "<reply-1@example.com>" {
		t.Fatalf("expected In-Reply-To from raw headers, got %s", params.InReplyTo)
	}
	if params.ThreadID != "thread-1" {
		t.Fatalf("expected thread-1, got %s", params.ThreadID)
	}

	var eventCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM events WHERE type = ?", EmailMessageTypeManualReply).Scan(&eventCount); err != nil {
		t.Fatalf("query manual reply events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 manual reply event, got %d", eventCount)
	}

	var saved EmailMessage
	if err := db.QueryRow(`
		SELECT campaign_id, lead_id, account_id, direction, type, step_number,
			message_id, thread_id, in_reply_to, from_email, to_emails, subject,
			text_body, display_body, occurred_at
		FROM email_messages
		WHERE message_id = ?`,
		"<manual-1@example.com>",
	).Scan(
		&saved.CampaignID,
		&saved.LeadID,
		&saved.AccountID,
		&saved.Direction,
		&saved.Type,
		&saved.StepNumber,
		&saved.MessageID,
		&saved.ThreadID,
		&saved.InReplyTo,
		&saved.FromEmail,
		&saved.ToEmails,
		&saved.Subject,
		&saved.TextBody,
		&saved.DisplayBody,
		&saved.OccurredAt,
	); err != nil {
		t.Fatalf("loading manual reply snapshot: %v", err)
	}
	if saved.Direction != EmailMessageDirectionOutbound {
		t.Fatalf("expected outbound snapshot, got %s", saved.Direction)
	}
	if saved.Type != EmailMessageTypeManualReply {
		t.Fatalf("expected manual reply snapshot, got %s", saved.Type)
	}
	if saved.TextBody != "Happy to send details." {
		t.Fatalf("expected saved body, got %q", saved.TextBody)
	}
	if saved.DisplayBody != "Happy to send details." {
		t.Fatalf("expected saved display body, got %q", saved.DisplayBody)
	}
	if !saved.OccurredAt.Equal(now) {
		t.Fatalf("expected occurred_at %s, got %s", now.Format(time.RFC3339), saved.OccurredAt.Format(time.RFC3339))
	}
}
