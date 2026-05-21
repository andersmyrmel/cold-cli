package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeDiscordNotifier struct {
	Events     []DiscordNotificationEvent
	FailOnCall int
}

func (f *fakeDiscordNotifier) NotifyDiscord(ctx context.Context, event DiscordNotificationEvent) error {
	if f.FailOnCall > 0 && len(f.Events)+1 == f.FailOnCall {
		return fmt.Errorf("discord unavailable")
	}
	f.Events = append(f.Events, event)
	return nil
}

func TestBuildDiscordWebhookPayloadDisablesMentionsAndTruncates(t *testing.T) {
	event := DiscordNotificationEvent{
		EventType:    "reply",
		Timestamp:    "2026-05-21T12:00:00Z",
		CampaignName: "v5-app-gap-lead-gift",
		LeadEmail:    "john@example.com",
		LeadCompany:  "Example",
		AccountEmail: "sender@example.com",
		FromEmail:    "John <john@example.com>",
		Subject:      "Re: @everyone",
		Snippet:      strings.Repeat("a", 600),
	}

	payload := BuildDiscordWebhookPayload(event)
	if len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("expected allowed_mentions.parse to be empty, got %#v", payload.AllowedMentions.Parse)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected one embed, got %d", len(payload.Embeds))
	}
	description := payload.Embeds[0].Description
	if len([]rune(description)) > 500 {
		t.Fatalf("expected truncated description, got %d runes", len([]rune(description)))
	}
	if !strings.HasSuffix(description, "...") {
		t.Fatalf("expected truncated description to end in ..., got %q", description)
	}
}

func TestListDiscordNotificationEvents(t *testing.T) {
	db := setupReplyTestDB(t)
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, 1, 'sent', 1, 'sent-1', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("insert sent event: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, 1, 'reply', 0, 'reply-1', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("insert reply event: %v", err)
	}
	insertInboundTestMessage(t, db, 1, 1, 1, "reply", "reply-1", "John <john@acme.com>", "Re: Hello", "Interested.")
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, 1, 'unsubscribe', 0, 'unsub-1', ?)`, time.Now().UTC()); err != nil {
		t.Fatalf("insert unsubscribe event: %v", err)
	}

	events, err := listDiscordNotificationEvents(db, 1, 10)
	if err != nil {
		t.Fatalf("listDiscordNotificationEvents error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 notification events, got %d", len(events))
	}
	if events[0].EventType != "reply" || events[0].FromEmail != "John <john@acme.com>" || events[0].Snippet != "Interested." {
		t.Fatalf("unexpected reply event: %#v", events[0])
	}
	if events[1].EventType != "unsubscribe" {
		t.Fatalf("expected unsubscribe event second, got %#v", events[1])
	}
}

func TestProcessDiscordNotificationsAdvancesCursorAfterSuccess(t *testing.T) {
	db := setupReplyTestDB(t)
	insertDiscordEvent(t, db, "reply", "reply-1")
	insertDiscordEvent(t, db, "unsubscribe", "unsub-1")

	notifier := &fakeDiscordNotifier{}
	notified, err := ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
	if err != nil {
		t.Fatalf("ProcessDiscordNotifications error: %v", err)
	}
	if notified != 2 || len(notifier.Events) != 2 {
		t.Fatalf("expected 2 notifications, got notified=%d events=%d", notified, len(notifier.Events))
	}

	lastID, ok, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if !ok || lastID != notifier.Events[1].EventID {
		t.Fatalf("expected cursor at last event %d, got ok=%v id=%d", notifier.Events[1].EventID, ok, lastID)
	}
}

func TestProcessDiscordNotificationsStopsOnFailure(t *testing.T) {
	db := setupReplyTestDB(t)
	firstID := insertDiscordEvent(t, db, "reply", "reply-1")
	insertDiscordEvent(t, db, "reply", "reply-2")

	notifier := &fakeDiscordNotifier{FailOnCall: 2}
	notified, err := ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
	if err == nil {
		t.Fatal("expected discord failure")
	}
	if notified != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected one successful notification before failure, got notified=%d events=%d", notified, len(notifier.Events))
	}

	lastID, ok, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if !ok || lastID != firstID {
		t.Fatalf("expected cursor to remain at first event %d, got ok=%v id=%d", firstID, ok, lastID)
	}
}

func TestDiscordWebhookNotifierPostsPayload(t *testing.T) {
	var payload discordWebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	notifier := DiscordWebhookNotifier{WebhookURL: server.URL, HTTPClient: server.Client()}
	err := notifier.NotifyDiscord(context.Background(), DiscordNotificationEvent{
		EventType:    "reply",
		CampaignName: "test",
		LeadEmail:    "john@acme.com",
		AccountEmail: "sender@x.com",
		Snippet:      "Interested",
	})
	if err != nil {
		t.Fatalf("NotifyDiscord error: %v", err)
	}
	if len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("expected mentions disabled, got %#v", payload.AllowedMentions.Parse)
	}
	if len(payload.Embeds) != 1 || payload.Embeds[0].Title != "New cold email reply" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestTickSendsDiscordNotificationForNewIMAPReply(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	if _, err := execDB(db, "UPDATE accounts SET provider = ? WHERE id = ?", AccountProviderSMTPIMAP, accountIDs[0]); err != nil {
		t.Fatalf("updating account provider: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (?, ?, ?, 'sent', 1, '<sent-1@example.com>', '<sent-1@example.com>')`,
		campaignID, leadIDs[0], accountIDs[0]); err != nil {
		t.Fatalf("insert sent event: %v", err)
	}
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 2, now.Add(24*time.Hour))

	imapMock := &MockIMAPMessageLister{
		Messages: []GWSMessage{{
			ID:        "<reply-1@example.com>",
			InReplyTo: "<sent-1@example.com>",
			From:      "John <john@acme.com>",
			To:        "sender@x.com",
			Subject:   "Re: Hello",
			Snippet:   "Interested.",
			TextBody:  "Interested.",
			Date:      now,
		}},
	}
	notifier := &fakeDiscordNotifier{}

	result, err := Tick(TickConfig{
		DB:              db,
		GWS:             &MockGWS{},
		IMAP:            imapMock,
		DiscordNotifier: notifier,
		Now:             now,
		NoSleep:         true,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.RepliesDetected != 1 {
		t.Fatalf("expected one reply, got %d", result.RepliesDetected)
	}
	if result.DiscordNotificationsSent != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected one discord notification, got result=%d events=%d", result.DiscordNotificationsSent, len(notifier.Events))
	}
	if notifier.Events[0].EventType != "reply" || notifier.Events[0].Subject != "Re: Hello" || notifier.Events[0].Snippet != "Interested." {
		t.Fatalf("unexpected discord event: %#v", notifier.Events[0])
	}
}

func TestTickDryRunPreservesDiscordNotificationForLater(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Now().UTC()

	if _, err := execDB(db, "UPDATE accounts SET provider = ? WHERE id = ?", AccountProviderSMTPIMAP, accountIDs[0]); err != nil {
		t.Fatalf("updating account provider: %v", err)
	}
	result, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
		VALUES (?, ?, ?, 'sent', 1, '<sent-1@example.com>', '<sent-1@example.com>')`,
		campaignID, leadIDs[0], accountIDs[0])
	if err != nil {
		t.Fatalf("insert sent event: %v", err)
	}
	sentEventID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	imapMock := &MockIMAPMessageLister{
		Messages: []GWSMessage{{
			ID:        "<reply-1@example.com>",
			InReplyTo: "<sent-1@example.com>",
			From:      "John <john@acme.com>",
			Subject:   "Re: Hello",
			Snippet:   "Interested.",
			Date:      now,
		}},
	}
	notifier := &fakeDiscordNotifier{}

	tickResult, err := Tick(TickConfig{
		DB:              db,
		GWS:             &MockGWS{},
		IMAP:            imapMock,
		DiscordNotifier: notifier,
		DryRun:          true,
		Now:             now,
		NoSleep:         true,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if tickResult.DiscordNotificationsSent != 0 || len(notifier.Events) != 0 {
		t.Fatalf("dry-run should not send discord notifications, got result=%d events=%d", tickResult.DiscordNotificationsSent, len(notifier.Events))
	}

	cursor, ok, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if !ok || cursor != sentEventID {
		t.Fatalf("expected dry-run cursor to stay at pre-poll sent event %d, got ok=%v id=%d", sentEventID, ok, cursor)
	}

	notified, err := ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
	if err != nil {
		t.Fatalf("ProcessDiscordNotifications error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected later notification for dry-run-detected reply, got notified=%d events=%d", notified, len(notifier.Events))
	}
}

func insertDiscordEvent(t *testing.T, db *sql.DB, eventType, messageID string) int64 {
	t.Helper()
	result, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, 1, ?, 0, ?, ?)`, eventType, messageID, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	insertInboundTestMessage(t, db, 1, 1, 1, eventType, messageID, "John <john@acme.com>", "Re: Hello", "Interested.")
	return id
}

func insertInboundTestMessage(t *testing.T, db *sql.DB, campaignID, leadID, accountID int64, messageType, messageID, from, subject, body string) {
	t.Helper()
	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: campaignID,
		LeadID:     leadID,
		AccountID:  accountID,
		Direction:  EmailMessageDirectionInbound,
		Type:       messageType,
		MessageID:  messageID,
		ThreadID:   messageID,
		FromEmail:  from,
		ToEmails:   "sender@x.com",
		Subject:    subject,
		TextBody:   body,
		Snippet:    body,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert inbound message: %v", err)
	}
}
