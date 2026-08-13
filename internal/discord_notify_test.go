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
		CampaignName: "example-campaign",
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

func TestBuildDiscordWebhookPayloadCampaignCompletionAndIdleReminder(t *testing.T) {
	completion := BuildDiscordWebhookPayload(DiscordNotificationEvent{
		EventType:         DiscordEventCampaignCompleted,
		Timestamp:         "2026-08-01T12:00:00Z",
		WorkspaceID:       "storeinspect",
		CampaignName:      "Shopify App Store Leads",
		CampaignStatus:    "completed",
		AccountEmails:     []string{"sender@example.com"},
		IdleAccountEmails: []string{"sender@example.com"},
		LeadsContacted:    10,
		SentCount:         20,
		ReplyCount:        1,
	})
	if len(completion.Embeds) != 1 || completion.Embeds[0].Title != "StoreInspect campaign finished" {
		t.Fatalf("unexpected campaign completion payload: %#v", completion)
	}
	if !strings.Contains(completion.Embeds[0].Description, "no pending sends") {
		t.Fatalf("expected actionable idle description, got %q", completion.Embeds[0].Description)
	}
	if len(completion.AllowedMentions.Parse) != 0 {
		t.Fatalf("expected mentions disabled, got %#v", completion.AllowedMentions.Parse)
	}

	idle := BuildDiscordWebhookPayload(DiscordNotificationEvent{
		EventType:    DiscordEventSenderIdle,
		WorkspaceID:  "storeinspect",
		AccountEmail: "sender@example.com",
		Reminder:     true,
	})
	if len(idle.Embeds) != 1 || idle.Embeds[0].Title != "StoreInspect sender is still idle" {
		t.Fatalf("unexpected idle reminder payload: %#v", idle)
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

	events, err := listDiscordNotificationEvents(db, 1, 10, nil)
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

func TestListDiscordNotificationEventsFiltersByProvider(t *testing.T) {
	db := setupReplyTestDB(t)
	if _, err := execDB(db, `UPDATE accounts SET provider = ? WHERE id = 1`, AccountProviderSMTPIMAP); err != nil {
		t.Fatalf("update smtp account: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO accounts (email, provider) VALUES ('gmail@example.com', ?)`, AccountProviderGWS); err != nil {
		t.Fatalf("insert gws account: %v", err)
	}

	insertDiscordEventForAccount(t, db, 1, "reply", "smtp-reply")
	insertDiscordEventForAccount(t, db, 2, "reply", "gws-reply")

	events, err := listDiscordNotificationEvents(db, 0, 10, []string{AccountProviderSMTPIMAP})
	if err != nil {
		t.Fatalf("listDiscordNotificationEvents error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 SMTP/IMAP event, got %d", len(events))
	}
	if events[0].MessageID != "smtp-reply" || events[0].AccountEmail != "sender@x.com" {
		t.Fatalf("unexpected filtered event: %#v", events[0])
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

	notifier := DiscordWebhookNotifier{
		WebhookURL: server.URL,
		Username:   "cold-cli Replies",
		AvatarURL:  "https://example.com/brand/logo.png",
		HTTPClient: server.Client(),
	}
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
	if payload.Username != "cold-cli Replies" {
		t.Fatalf("expected custom username, got %q", payload.Username)
	}
	if payload.AvatarURL != "https://example.com/brand/logo.png" {
		t.Fatalf("expected custom avatar URL, got %q", payload.AvatarURL)
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

func TestTickDryRunLeavesReplyNotificationForRealTick(t *testing.T) {
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
	if tickResult.RepliesDetected != 0 {
		t.Fatalf("dry-run should not poll replies, got replies=%d", tickResult.RepliesDetected)
	}

	cursor, ok, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if !ok || cursor != sentEventID {
		t.Fatalf("expected dry-run cursor to stay at pre-poll sent event %d, got ok=%v id=%d", sentEventID, ok, cursor)
	}

	var replyEvents int
	if err := queryRowDB(db, "SELECT COUNT(*) FROM events WHERE type = 'reply'").Scan(&replyEvents); err != nil {
		t.Fatalf("count reply events: %v", err)
	}
	if replyEvents != 0 {
		t.Fatalf("expected no reply event after dry-run, got %d", replyEvents)
	}

	tickResult, err = Tick(TickConfig{
		DB:              db,
		GWS:             &MockGWS{},
		IMAP:            imapMock,
		DiscordNotifier: notifier,
		Now:             now,
		NoSleep:         true,
	})
	if err != nil {
		t.Fatalf("real tick error: %v", err)
	}
	if tickResult.RepliesDetected != 1 {
		t.Fatalf("expected real tick to detect reply, got %d", tickResult.RepliesDetected)
	}
	if tickResult.DiscordNotificationsSent != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected real tick notification, got result=%d events=%d", tickResult.DiscordNotificationsSent, len(notifier.Events))
	}
}

func TestProcessDiscordOperationalNotificationsCampaignCompletionIncludesIdleInbox(t *testing.T) {
	db := setupReplyTestDB(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := execDB(db, "UPDATE accounts SET workspace_id = 'storeinspect' WHERE id = 1"); err != nil {
		t.Fatalf("update account workspace: %v", err)
	}
	if _, err := execDB(db, `UPDATE campaigns
		SET workspace_id = 'storeinspect', status = 'completed', completed_at = ?
		WHERE id = 1`, now.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("complete campaign: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO scheduled_sends
		(campaign_id, lead_id, account_id, step_number, send_at, status, sent_at)
		VALUES (1, 1, 1, 1, ?, 'sent', ?)`, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert sent send: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO events
		(campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, 1, 'sent', 1, 'sent-1', ?)`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert sent event: %v", err)
	}

	notifier := &fakeDiscordNotifier{}
	notified, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, DiscordOperationalNotifyOptions{
		Workspaces: []string{"storeinspect"},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("ProcessDiscordOperationalNotifications error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected one combined completion notification, got notified=%d events=%d", notified, len(notifier.Events))
	}
	event := notifier.Events[0]
	if event.EventType != DiscordEventCampaignCompleted || event.WorkspaceID != "storeinspect" {
		t.Fatalf("unexpected completion event: %#v", event)
	}
	if event.CampaignName != "test" || event.LeadsContacted != 1 || event.SentCount != 1 {
		t.Fatalf("unexpected completion stats: %#v", event)
	}
	if len(event.AccountEmails) != 1 || event.AccountEmails[0] != "sender@x.com" {
		t.Fatalf("unexpected campaign inboxes: %#v", event.AccountEmails)
	}
	if len(event.IdleAccountEmails) != 1 || event.IdleAccountEmails[0] != "sender@x.com" {
		t.Fatalf("expected completed campaign inbox to be marked idle, got %#v", event.IdleAccountEmails)
	}

	notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, DiscordOperationalNotifyOptions{
		Workspaces: []string{"storeinspect"},
		Now:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second ProcessDiscordOperationalNotifications error: %v", err)
	}
	if notified != 0 || len(notifier.Events) != 1 {
		t.Fatalf("expected completion and idle state to be deduplicated, got notified=%d events=%d", notified, len(notifier.Events))
	}
}

func TestProcessDiscordOperationalNotificationsIdleLifecycleAndReminder(t *testing.T) {
	db := setupReplyTestDB(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := execDB(db, "UPDATE accounts SET workspace_id = 'storeinspect' WHERE id = 1"); err != nil {
		t.Fatalf("update account workspace: %v", err)
	}
	if _, err := execDB(db, "UPDATE campaigns SET workspace_id = 'storeinspect' WHERE id = 1"); err != nil {
		t.Fatalf("update campaign workspace: %v", err)
	}

	notifier := &fakeDiscordNotifier{}
	opts := DiscordOperationalNotifyOptions{Workspaces: []string{"storeinspect"}, Now: now}
	notified, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err != nil {
		t.Fatalf("first idle notification error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 1 || notifier.Events[0].EventType != DiscordEventSenderIdle {
		t.Fatalf("expected one sender-idle notification, got notified=%d events=%#v", notified, notifier.Events)
	}
	if notifier.Events[0].Reminder {
		t.Fatal("first idle notification should not be a reminder")
	}

	opts.Now = now.Add(time.Hour)
	notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err != nil {
		t.Fatalf("deduplicated idle check error: %v", err)
	}
	if notified != 0 || len(notifier.Events) != 1 {
		t.Fatalf("expected no repeat inside reminder interval, got notified=%d events=%d", notified, len(notifier.Events))
	}

	opts.Now = now.Add(25 * time.Hour)
	notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err != nil {
		t.Fatalf("idle reminder error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 2 || !notifier.Events[1].Reminder {
		t.Fatalf("expected one daily idle reminder, got notified=%d events=%#v", notified, notifier.Events)
	}

	if _, err := execDB(db, `INSERT INTO scheduled_sends
		(campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (1, 1, 1, 1, ?, 'pending')`, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("insert pending send: %v", err)
	}
	opts.Now = now.Add(26 * time.Hour)
	if _, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts); err != nil {
		t.Fatalf("clear idle state error: %v", err)
	}
	if _, err := execDB(db, "UPDATE scheduled_sends SET status = 'cancelled'"); err != nil {
		t.Fatalf("cancel pending send: %v", err)
	}
	opts.Now = now.Add(27 * time.Hour)
	notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err != nil {
		t.Fatalf("new idle transition error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 3 || notifier.Events[2].Reminder {
		t.Fatalf("expected a fresh idle alert after work resumed, got notified=%d events=%#v", notified, notifier.Events)
	}
}

func TestProcessDiscordOperationalNotificationsRetriesFailedWebhook(t *testing.T) {
	db := setupReplyTestDB(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := execDB(db, "UPDATE accounts SET workspace_id = 'storeinspect' WHERE id = 1"); err != nil {
		t.Fatalf("update account workspace: %v", err)
	}

	notifier := &fakeDiscordNotifier{FailOnCall: 1}
	opts := DiscordOperationalNotifyOptions{Workspaces: []string{"storeinspect"}, Now: now}
	notified, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err == nil || notified != 0 {
		t.Fatalf("expected initial webhook failure, got notified=%d err=%v", notified, err)
	}

	notifier.FailOnCall = 0
	notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
	if err != nil {
		t.Fatalf("retry error: %v", err)
	}
	if notified != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected failed idle alert to retry once, got notified=%d events=%d", notified, len(notifier.Events))
	}
}

func TestProcessDiscordOperationalNotificationsScopesIdleAlertsByWorkspace(t *testing.T) {
	db := setupReplyTestDB(t)
	notifier := &fakeDiscordNotifier{}
	notified, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, DiscordOperationalNotifyOptions{
		Workspaces: []string{"storeinspect"},
		Now:        time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ProcessDiscordOperationalNotifications error: %v", err)
	}
	if notified != 0 || len(notifier.Events) != 0 {
		t.Fatalf("default-workspace inbox should not alert StoreInspect, got notified=%d events=%#v", notified, notifier.Events)
	}
}

func TestTickNotifiesCompletionBeforeNoActiveCampaignEarlyExit(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := execDB(db, "UPDATE accounts SET workspace_id = 'storeinspect' WHERE id = ?", accountIDs[0]); err != nil {
		t.Fatalf("update account workspace: %v", err)
	}
	if _, err := execDB(db, "UPDATE campaigns SET workspace_id = 'storeinspect' WHERE id = ?", campaignID); err != nil {
		t.Fatalf("update campaign workspace: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO scheduled_sends
		(campaign_id, lead_id, account_id, step_number, send_at, status, sent_at)
		VALUES (?, ?, ?, 1, ?, 'sent', ?)`, campaignID, leadIDs[0], accountIDs[0], now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert terminal send: %v", err)
	}

	notifier := &fakeDiscordNotifier{}
	result, err := Tick(TickConfig{
		DB:                           db,
		GWS:                          &MockGWS{},
		DiscordNotifier:              notifier,
		DiscordOperationalWorkspaces: []string{"storeinspect"},
		Now:                          now,
		NoSleep:                      true,
	})
	if err != nil {
		t.Fatalf("tick error: %v", err)
	}
	if result.DiscordNotificationsSent != 1 || len(notifier.Events) != 1 {
		t.Fatalf("expected completion alert before early exit, got result=%d events=%#v", result.DiscordNotificationsSent, notifier.Events)
	}
	if notifier.Events[0].EventType != DiscordEventCampaignCompleted {
		t.Fatalf("expected campaign completion event, got %#v", notifier.Events[0])
	}
}

func TestTickDryRunDefersOperationalCompletionNotification(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	if _, err := execDB(db, "UPDATE accounts SET workspace_id = 'storeinspect' WHERE id = ?", accountIDs[0]); err != nil {
		t.Fatalf("update account workspace: %v", err)
	}
	if _, err := execDB(db, "UPDATE campaigns SET workspace_id = 'storeinspect' WHERE id = ?", campaignID); err != nil {
		t.Fatalf("update campaign workspace: %v", err)
	}
	if _, err := execDB(db, `INSERT INTO scheduled_sends
		(campaign_id, lead_id, account_id, step_number, send_at, status, sent_at)
		VALUES (?, ?, ?, 1, ?, 'sent', ?)`, campaignID, leadIDs[0], accountIDs[0], now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert terminal send: %v", err)
	}

	notifier := &fakeDiscordNotifier{}
	cfg := TickConfig{
		DB:                           db,
		GWS:                          &MockGWS{},
		DiscordNotifier:              notifier,
		DiscordOperationalWorkspaces: []string{"storeinspect"},
		Now:                          now,
		NoSleep:                      true,
		DryRun:                       true,
	}
	result, err := Tick(cfg)
	if err != nil {
		t.Fatalf("dry-run tick error: %v", err)
	}
	if result.DiscordNotificationsSent != 0 || len(notifier.Events) != 0 {
		t.Fatalf("dry-run should not send operational notifications, got result=%d events=%#v", result.DiscordNotificationsSent, notifier.Events)
	}

	cfg.DryRun = false
	result, err = Tick(cfg)
	if err != nil {
		t.Fatalf("real tick error: %v", err)
	}
	if result.DiscordNotificationsSent != 1 || len(notifier.Events) != 1 {
		t.Fatalf("real tick should deliver deferred completion, got result=%d events=%#v", result.DiscordNotificationsSent, notifier.Events)
	}
}

func TestBuildDiscordWebhookPayloadForInboxReconciliationFailure(t *testing.T) {
	payload := BuildDiscordWebhookPayload(DiscordNotificationEvent{
		EventType:   DiscordEventInboxReconciliationFailed,
		WorkspaceID: "storeinspect",
		Timestamp:   "2026-08-13T03:00:00Z",
		Snippet:     "provider reconciliation incomplete: 2 messages remain untracked",
	})
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected one embed, got %+v", payload)
	}
	embed := payload.Embeds[0]
	if embed.Title != "StoreInspect inbox reconciliation failed" {
		t.Fatalf("unexpected title: %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "2 messages remain untracked") || embed.Color != 0xef4444 {
		t.Fatalf("unexpected reconciliation alert: %+v", embed)
	}
	if len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("mentions must remain disabled: %+v", payload.AllowedMentions)
	}
}

func insertDiscordEvent(t *testing.T, db *sql.DB, eventType, messageID string) int64 {
	t.Helper()
	return insertDiscordEventForAccount(t, db, 1, eventType, messageID)
}

func insertDiscordEventForAccount(t *testing.T, db *sql.DB, accountID int64, eventType, messageID string) int64 {
	t.Helper()
	result, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp)
		VALUES (1, 1, ?, ?, 0, ?, ?)`, accountID, eventType, messageID, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	insertInboundTestMessage(t, db, 1, 1, accountID, eventType, messageID, "John <john@acme.com>", "Re: Hello", "Interested.")
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
