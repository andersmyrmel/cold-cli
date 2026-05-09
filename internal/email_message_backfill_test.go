package internal

import (
	"testing"
	"time"
)

func TestBackfillEmailMessages_BackfillsInboundAndRelatedSentGWSMessages(t *testing.T) {
	db := setupReplyTestDB(t)
	now := time.Date(2026, time.May, 6, 12, 0, 0, 0, time.UTC)

	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
		VALUES (1, 1, 1, 'sent', 1, '<sent-1@example.com>', 'thread-1', ?)`, now.Add(-10*time.Minute))
	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
		VALUES (1, 1, 1, 'reply', 0, 'gmail-reply-1', 'thread-1', ?)`, now)

	mock := &MockGWS{
		InboxMessages: []GWSMessage{
			{
				ID:       "gmail-sent-1",
				ThreadID: "thread-1",
				From:     "sender@x.com",
				To:       "john@acme.com",
				Subject:  "Hi John",
				Snippet:  "Hello John",
				TextBody: "Hello John",
				Headers: map[string]string{
					"Message-ID": "<sent-1@example.com>",
				},
			},
			{
				ID:        "gmail-reply-1",
				ThreadID:  "thread-1",
				From:      "John <john@acme.com>",
				To:        "sender@x.com",
				Subject:   "Re: Hi John",
				Snippet:   "Interested",
				TextBody:  "Interested, send details.",
				InReplyTo: "<sent-1@example.com>",
				Headers: map[string]string{
					"Message-ID":  "<reply-1@example.com>",
					"In-Reply-To": "<sent-1@example.com>",
				},
			},
		},
	}

	result, err := BackfillEmailMessages(BackfillEmailMessagesConfig{
		DB:          db,
		GWS:         mock,
		Limit:       10,
		IncludeSent: true,
	})
	if err != nil {
		t.Fatalf("BackfillEmailMessages error: %v", err)
	}
	if result.Scanned != 1 {
		t.Fatalf("expected 1 scanned inbound event, got %d", result.Scanned)
	}
	if result.Backfilled != 2 {
		t.Fatalf("expected 2 backfilled messages, got %d", result.Backfilled)
	}
	if result.Inbound != 1 || result.Sent != 1 {
		t.Fatalf("expected inbound=1 sent=1, got inbound=%d sent=%d", result.Inbound, result.Sent)
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
		t.Fatalf("expected 2 thread messages, got %d", len(messages))
	}
	if messages[0].Direction != EmailMessageDirectionOutbound || messages[0].TextBody != "Hello John" {
		t.Fatalf("expected outbound sent snapshot first, got direction=%s body=%q", messages[0].Direction, messages[0].TextBody)
	}
	if messages[1].Direction != EmailMessageDirectionInbound || messages[1].TextBody != "Interested, send details." {
		t.Fatalf("expected inbound reply snapshot second, got direction=%s body=%q", messages[1].Direction, messages[1].TextBody)
	}
	if len(mock.ListCalls) != 1 || mock.ListCalls[0].Query != "rfc822msgid:sent-1@example.com" {
		t.Fatalf("expected sent lookup by rfc822msgid, got %+v", mock.ListCalls)
	}
}

func TestBackfillEmailMessages_DryRunDoesNotInsert(t *testing.T) {
	db := setupReplyTestDB(t)
	now := time.Date(2026, time.May, 6, 12, 0, 0, 0, time.UTC)

	db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
		VALUES (1, 1, 1, 'reply', 0, 'gmail-reply-1', 'thread-1', ?)`, now)

	result, err := BackfillEmailMessages(BackfillEmailMessagesConfig{
		DB:     db,
		GWS:    &MockGWS{InboxMessages: []GWSMessage{{ID: "gmail-reply-1", ThreadID: "thread-1", TextBody: "Reply"}}},
		Limit:  10,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("BackfillEmailMessages error: %v", err)
	}
	if result.Backfilled != 1 || !result.DryRun {
		t.Fatalf("expected dry-run backfilled=1, got %+v", result)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM email_messages").Scan(&count); err != nil {
		t.Fatalf("count email_messages: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no inserts in dry-run, got %d", count)
	}
}
