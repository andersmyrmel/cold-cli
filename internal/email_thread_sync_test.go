package internal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func seedStoredReplyThread(t *testing.T, provider string) (*MockGWS, *MockIMAPMessageLister, *Store, int64, int64) {
	t.Helper()

	t.Setenv("COLD_CLI_DATA_DIR", t.TempDir())
	t.Setenv("COLD_CLI_DATABASE_URL", "")
	store, err := OpenStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	db := store.DB
	result, err := db.Exec(`INSERT INTO accounts (
		workspace_id, email, provider, status,
		smtp_host, smtp_port, smtp_username, smtp_password_ref, smtp_tls_mode,
		imap_host, imap_port, imap_username, imap_password_ref, imap_tls_mode
	) VALUES ('storeinspect', 'sender@example.com', ?, 'active',
		'smtp.example.com', 587, 'sender@example.com', 'env:MAIL_PASSWORD', 'starttls',
		'imap.example.com', 993, 'sender@example.com', 'env:MAIL_PASSWORD', 'ssl')`, provider)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	accountID, _ := result.LastInsertId()

	result, err = db.Exec(`INSERT INTO campaigns (
		workspace_id, name, status, sequence_file, sequence_content
	) VALUES ('storeinspect', 'sync-test', 'active', 'sequence.yml', 'defaults:\n  from_name: Anders')`)
	if err != nil {
		t.Fatalf("insert campaign: %v", err)
	}
	campaignID, _ := result.LastInsertId()

	result, err = db.Exec(`INSERT INTO leads (email, first_name, domain)
		VALUES ('lead@example.net', 'Lead', 'example.net')`)
	if err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	leadID, _ := result.LastInsertId()

	if _, err := db.Exec(`INSERT INTO campaign_leads (campaign_id, lead_id, status)
		VALUES (?, ?, 'replied')`, campaignID, leadID); err != nil {
		t.Fatalf("insert campaign lead: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO campaign_accounts (campaign_id, account_id)
		VALUES (?, ?)`, campaignID, accountID); err != nil {
		t.Fatalf("insert campaign account: %v", err)
	}

	rootAt := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	replyAt := rootAt.Add(24 * time.Hour)
	if _, err := db.Exec(`INSERT INTO events (
		campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp
	) VALUES (?, ?, ?, 'sent', 1, '<root@example.com>', 'thread-1', ?),
		(?, ?, ?, 'reply', 0, '<reply@example.net>', 'thread-1', ?)`,
		campaignID, leadID, accountID, rootAt,
		campaignID, leadID, accountID, replyAt); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO email_messages (
		campaign_id, lead_id, account_id, direction, type, step_number,
		message_id, thread_id, in_reply_to, from_email, to_emails, subject,
		text_body, display_body, snippet, raw_headers, occurred_at
	) VALUES
		(?, ?, ?, 'outbound', 'sent', 1, '<root@example.com>', 'thread-1', '',
		 'sender@example.com', 'lead@example.net', 'Question', 'Initial', 'Initial', 'Initial',
		 '{"Message-ID":"<root@example.com>"}', ?),
		(?, ?, ?, 'inbound', 'reply', 0, '<reply@example.net>', 'thread-1', '<root@example.com>',
		 'lead@example.net', 'sender@example.com', 'Re: Question', 'Interested', 'Interested', 'Interested',
		 '{"Message-ID":"<reply@example.net>","References":"<root@example.com>"}', ?)`,
		campaignID, leadID, accountID, rootAt,
		campaignID, leadID, accountID, replyAt); err != nil {
		t.Fatalf("insert email messages: %v", err)
	}

	return &MockGWS{}, &MockIMAPMessageLister{}, store, campaignID, leadID
}

func TestSyncEmailThreadGWSImportsExternallySentMessage(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	externalAt := time.Date(2026, time.January, 4, 12, 0, 0, 0, time.UTC)
	gws.InboxMessages = []GWSMessage{
		{
			ID: "gmail-root", ThreadID: "thread-1", From: "sender@example.com", To: "lead@example.net",
			Subject: "Question", TextBody: "Initial", Date: externalAt.Add(-48 * time.Hour),
			Headers: map[string]string{"Message-ID": "<root@example.com>"},
		},
		{
			ID: "gmail-reply", ThreadID: "thread-1", From: "lead@example.net", To: "sender@example.com",
			Subject: "Re: Question", TextBody: "Interested", InReplyTo: "<root@example.com>", Date: externalAt.Add(-24 * time.Hour),
			Headers: map[string]string{"Message-ID": "<reply@example.net>", "References": "<root@example.com>"},
		},
		{
			ID: "gmail-manual", ThreadID: "thread-1", From: "Sender <sender@example.com>", To: "lead@example.net",
			Subject: "Re: Question", TextBody: "Sent manually in Gmail", InReplyTo: "<reply@example.net>", Date: externalAt,
			Headers: map[string]string{"Message-ID": "<manual@gmail.example>", "References": "<root@example.com> <reply@example.net>"},
		},
	}

	result, err := SyncEmailThread(SyncEmailThreadConfig{
		DB: store.DB, WorkspaceID: "storeinspect", CampaignID: campaignID, LeadID: leadID, GWS: gws,
	})
	if err != nil {
		t.Fatalf("sync thread: %v", err)
	}
	if result.Added != 1 || result.OutboundAdded != 1 || result.InboundAdded != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(gws.ThreadCalls) != 1 || gws.ThreadCalls[0].ThreadID != "thread-1" {
		t.Fatalf("unexpected thread calls: %+v", gws.ThreadCalls)
	}

	messages, err := ListEmailThreadMessages(store.DB, ListEmailThreadMessagesOpts{
		CampaignID: campaignID, LeadID: leadID, ThreadID: "thread-1",
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	latest := messages[len(messages)-1]
	if latest.Direction != EmailMessageDirectionOutbound || latest.Type != EmailMessageTypeManualReply {
		t.Fatalf("unexpected latest message: %+v", latest)
	}
	if latest.TextBody != "Sent manually in Gmail" {
		t.Fatalf("unexpected latest body: %q", latest.TextBody)
	}
}

func TestSyncEmailThreadSMTPImportsSentAndFiltersUnrelatedMessages(t *testing.T) {
	_, imap, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderSMTPIMAP)
	manualAt := time.Date(2026, time.January, 5, 12, 0, 0, 0, time.UTC)
	imap.Messages = []GWSMessage{
		{
			ID: "<manual@sender.example>", ThreadID: "<root@example.com>",
			From: "sender@example.com", To: "lead@example.net", Subject: "Re: Question",
			TextBody: "Sent manually in Migadu", InReplyTo: "<reply@example.net>", Date: manualAt,
			Headers: map[string]string{"Message-ID": "<manual@sender.example>", "References": "<root@example.com> <reply@example.net>"},
		},
		{
			ID: "<unrelated@example.org>", ThreadID: "<other-root@example.org>",
			From: "someone@example.org", To: "sender@example.com", Subject: "Unrelated",
			TextBody: "Do not import", Date: manualAt,
			Headers: map[string]string{"Message-ID": "<unrelated@example.org>", "References": "<other-root@example.org>"},
		},
	}

	result, err := SyncEmailThread(SyncEmailThreadConfig{
		DB: store.DB, WorkspaceID: "storeinspect", CampaignID: campaignID, LeadID: leadID, IMAP: imap,
	})
	if err != nil {
		t.Fatalf("sync thread: %v", err)
	}
	if result.Fetched != 2 || result.Matched != 1 || result.Added != 1 || result.OutboundAdded != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(imap.ListCalls) != 1 {
		t.Fatalf("expected one IMAP call, got %+v", imap.ListCalls)
	}

	messages, err := ListEmailThreadMessages(store.DB, ListEmailThreadMessagesOpts{
		CampaignID: campaignID, LeadID: leadID, ThreadID: "thread-1",
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
	if messages[2].TextBody != "Sent manually in Migadu" {
		t.Fatalf("unexpected latest body: %q", messages[2].TextBody)
	}
}

func TestSendInboxReplyRefreshFailureBlocksDelivery(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	gws.ThreadError = errors.New("gmail unavailable")

	_, err := SendInboxReply(SendInboxReplyConfig{
		DB: store.DB, WorkspaceID: "storeinspect", CampaignID: campaignID, LeadID: leadID,
		Body: "This must not send", RefreshThread: true, GWS: gws,
	})
	if err == nil || !strings.Contains(err.Error(), "refreshing thread") {
		t.Fatalf("expected refresh error, got %v", err)
	}
	if len(gws.SentEmails) != 0 {
		t.Fatalf("expected no sends, got %+v", gws.SentEmails)
	}
}

func TestSendInboxReplyRefreshBlocksNewlyDiscoveredUnsubscribe(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	gws.InboxMessages = []GWSMessage{
		{
			ID: "gmail-unsubscribe", ThreadID: "thread-1", From: "lead@example.net", To: "sender@example.com",
			Subject: "Re: Question", TextBody: "Please unsubscribe me", InReplyTo: "<reply@example.net>",
			Date:    time.Date(2026, time.January, 5, 12, 0, 0, 0, time.UTC),
			Headers: map[string]string{"Message-ID": "<unsubscribe@example.net>", "References": "<root@example.com> <reply@example.net>"},
		},
	}

	_, err := SendInboxReply(SendInboxReplyConfig{
		DB: store.DB, WorkspaceID: "storeinspect", CampaignID: campaignID, LeadID: leadID,
		Body: "This must not send", RefreshThread: true, GWS: gws,
	})
	if err == nil || !strings.Contains(err.Error(), "unsubscribe") {
		t.Fatalf("expected unsubscribe block, got %v", err)
	}
	if len(gws.SentEmails) != 0 {
		t.Fatalf("expected no sends, got %+v", gws.SentEmails)
	}
}
