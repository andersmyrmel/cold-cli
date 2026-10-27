package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscordWorkspaceRouting(t *testing.T) {
	for _, dialect := range []Dialect{DialectSQLite, DialectPostgres} {
		t.Run(string(dialect), func(t *testing.T) {
			db := setupDiscordWorkspaceDB(t)
			registerDBDialect(db, dialect)
			t.Cleanup(func() { unregisterDBDialect(db) })

			requests := make(chan string, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload discordWebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode notification: %v", err)
				}
				workspace := r.URL.Path[1:]
				if len(payload.Embeds) != 1 || payload.Embeds[0].Description != "Example response for "+workspace {
					t.Errorf("notification reached the wrong workspace: path=%s payload=%#v", r.URL.Path, payload)
				}
				requests <- r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			notifier := DiscordWebhookNotifier{
				WebhookURL: server.URL + "/shared",
				WorkspaceWebhookURLs: map[string]string{
					"workspace-a": server.URL + "/workspace-a",
					"workspace-b": server.URL + "/workspace-b",
				},
			}
			insertDiscordWorkspaceEvent(t, db, 3, "reply", "unmapped-first")
			insertDiscordWorkspaceEvent(t, db, 1, "reply", "reply-a")
			insertDiscordWorkspaceEvent(t, db, 2, "unsubscribe", "unsubscribe-b")
			lastID := insertDiscordWorkspaceEvent(t, db, 3, "unsubscribe", "unmapped-last")

			notified, err := ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
			if err != nil || notified != 2 || len(requests) != 2 {
				t.Fatalf("expected two routed notifications: notified=%d requests=%d err=%v", notified, len(requests), err)
			}
			if first, second := <-requests, <-requests; first != "/workspace-a" || second != "/workspace-b" {
				t.Fatalf("unexpected routes: %s, %s", first, second)
			}
			cursor, _, err := getKVInt64(db, discordNotifyLastEventIDKey)
			if err != nil || cursor != lastID {
				t.Fatalf("cursor must advance past unmapped events: cursor=%d want=%d err=%v", cursor, lastID, err)
			}
			notifier.WorkspaceWebhookURLs["workspace-c"] = server.URL + "/workspace-c"
			notified, err = ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
			if err != nil || notified != 0 || len(requests) != 0 {
				t.Fatalf("adding a route must not replay skipped replies: notified=%d err=%v", notified, err)
			}
			insertDiscordWorkspaceEvent(t, db, 3, "reply", "new-reply-c")
			notified, err = ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
			if err != nil || notified != 1 || len(requests) != 1 || <-requests != "/workspace-c" {
				t.Fatalf("new replies should use the added route: notified=%d err=%v", notified, err)
			}
		})
	}
}

func TestDiscordWorkspaceRoutingRetriesFailedDestination(t *testing.T) {
	db := setupDiscordWorkspaceDB(t)
	firstID := insertDiscordWorkspaceEvent(t, db, 1, "reply", "reply-a")
	insertDiscordWorkspaceEvent(t, db, 2, "reply", "reply-b")
	lastID := insertDiscordWorkspaceEvent(t, db, 3, "reply", "unmapped-c")
	var fail atomic.Bool
	fail.Store(true)
	requests := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/b" && fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier := DiscordWebhookNotifier{WorkspaceWebhookURLs: map[string]string{
		"workspace-a": server.URL + "/a", "workspace-b": server.URL + "/b",
	}}
	notified, err := ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
	if err == nil || notified != 1 {
		t.Fatalf("expected one success before the failed route: notified=%d err=%v", notified, err)
	}
	cursor, _, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil || cursor != firstID {
		t.Fatalf("failed route advanced the cursor: cursor=%d want=%d err=%v", cursor, firstID, err)
	}
	fail.Store(false)
	notified, err = ProcessDiscordNotifications(context.Background(), db, notifier, DiscordNotifyOptions{})
	if err != nil || notified != 1 || len(requests) != 3 {
		t.Fatalf("expected only the failed destination to retry: notified=%d requests=%d err=%v", notified, len(requests), err)
	}
	for i, want := range []string{"/a", "/b", "/b"} {
		if got := <-requests; got != want {
			t.Fatalf("request %d: got %s want %s", i, got, want)
		}
	}
	cursor, _, err = getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil || cursor != lastID {
		t.Fatalf("retry did not finish the batch: cursor=%d want=%d err=%v", cursor, lastID, err)
	}
}

func TestDiscordOperationalWorkspaceRouting(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%v", completed), func(t *testing.T) {
			db := setupDiscordWorkspaceDB(t)
			now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
			if completed {
				if _, err := execDB(db, "UPDATE campaigns SET status = 'completed', completed_at = ?", now.Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			}
			requests := make(chan string, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			notifier := DiscordWebhookNotifier{
				WebhookURL: server.URL + "/shared",
				WorkspaceWebhookURLs: map[string]string{
					"workspace-a": server.URL + "/a", "workspace-b": server.URL + "/b",
				},
			}
			opts := DiscordOperationalNotifyOptions{Workspaces: []string{"*"}, Now: now}
			notified, err := ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
			if err != nil || notified != 2 || len(requests) != 2 {
				t.Fatalf("expected two operational alerts: notified=%d requests=%d err=%v", notified, len(requests), err)
			}
			for _, want := range []string{"/a", "/b"} {
				if got := <-requests; got != want {
					t.Fatalf("wrong operational route: got %s want %s", got, want)
				}
			}
			var idleNotified, completionNotified sql.NullString
			if err := queryRowDB(db, "SELECT idle_notified_at FROM accounts WHERE id = 3").Scan(&idleNotified); err != nil {
				t.Fatal(err)
			}
			if err := queryRowDB(db, "SELECT completion_notified_at FROM campaigns WHERE id = 3").Scan(&completionNotified); err != nil {
				t.Fatal(err)
			}
			if idleNotified.Valid || completionNotified.Valid {
				t.Fatal("unmapped operational alerts must not be marked as delivered")
			}
			notifier.WorkspaceWebhookURLs["workspace-c"] = server.URL + "/c"
			opts.Now = now.Add(time.Hour)
			notified, err = ProcessDiscordOperationalNotifications(context.Background(), db, notifier, opts)
			if err != nil || notified != 1 || len(requests) != 1 || <-requests != "/c" {
				t.Fatalf("adding a route should deliver the pending operational alert: notified=%d err=%v", notified, err)
			}
		})
	}
}

func TestDiscordWebhookRoutingNeverFallsBack(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	for _, routes := range []map[string]string{{}, {"workspace-a": ""}, {"workspace-b": server.URL}} {
		notifier := DiscordWebhookNotifier{WebhookURL: server.URL, WorkspaceWebhookURLs: routes}
		for _, eventType := range []string{"reply", "unsubscribe", DiscordEventCampaignCompleted, DiscordEventSenderIdle, DiscordEventInboxReconciliationFailed} {
			err := notifier.NotifyDiscord(context.Background(), DiscordNotificationEvent{WorkspaceID: "workspace-a", EventType: eventType})
			if !errors.Is(err, ErrDiscordWorkspaceUnconfigured) {
				t.Fatalf("expected unmapped workspace to be skipped, got %v", err)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("unmapped alerts reached the shared webhook")
	}
}

func TestDiscordNotificationSnapshotsStayWithinWorkspace(t *testing.T) {
	for _, dialect := range []Dialect{DialectSQLite, DialectPostgres} {
		t.Run(string(dialect), func(t *testing.T) {
			db := setupDiscordWorkspaceDB(t)
			registerDBDialect(db, dialect)
			t.Cleanup(func() { unregisterDBDialect(db) })
			for id := 1; id <= 3; id++ {
				insertDiscordWorkspaceEvent(t, db, id, "reply", "shared-provider-message-id")
				if _, err := execDB(db, "UPDATE email_messages SET from_email = ?, subject = ? WHERE campaign_id = ?",
					fmt.Sprintf("contact-%d@example.com", id), fmt.Sprintf("Example subject %d", id), id); err != nil {
					t.Fatal(err)
				}
			}
			events, err := listDiscordNotificationEvents(db, 0, 10, nil)
			if err != nil || len(events) != 3 {
				t.Fatalf("expected three reply events: events=%d err=%v", len(events), err)
			}
			for i, event := range events {
				id := i + 1
				workspace := fmt.Sprintf("workspace-%c", 'a'+i)
				if event.CampaignID != int64(id) || event.WorkspaceID != workspace ||
					event.FromEmail != fmt.Sprintf("contact-%d@example.com", id) ||
					event.Subject != fmt.Sprintf("Example subject %d", id) ||
					event.Snippet != "Example response for "+workspace {
					t.Fatalf("reply snapshot mixed workspaces: %#v", event)
				}
			}
		})
	}
}

func setupDiscordWorkspaceDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	if _, err := execDB(db, "INSERT INTO leads (email, first_name, domain) VALUES ('lead@example.com', 'Alex', 'example.com')"); err != nil {
		t.Fatal(err)
	}
	for i, workspace := range []string{"workspace-a", "workspace-b", "workspace-c"} {
		id := i + 1
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO accounts (id, workspace_id, email) VALUES (?, ?, ?)", []any{id, workspace, "sender@" + workspace + ".example"}},
			{"INSERT INTO campaigns (id, workspace_id, name, status, sequence_file) VALUES (?, ?, ?, 'active', 'example.yml')", []any{id, workspace, "Example " + workspace}},
			{"INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", []any{id, id}},
		} {
			if _, err := execDB(db, statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func insertDiscordWorkspaceEvent(t *testing.T, db *sql.DB, campaignID int, eventType, messageID string) int64 {
	t.Helper()
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id)
		VALUES (?, 1, ?, ?, 0, ?)`, campaignID, campaignID, eventType, messageID); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := queryRowDB(db, "SELECT id FROM events WHERE campaign_id = ? AND message_id = ?", campaignID, messageID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	workspace := fmt.Sprintf("workspace-%c", 'a'+campaignID-1)
	insertInboundTestMessage(t, db, int64(campaignID), 1, int64(campaignID), eventType, messageID,
		"Alex <lead@example.com>", "Re: Example inquiry", "Example response for "+workspace)
	return eventID
}
