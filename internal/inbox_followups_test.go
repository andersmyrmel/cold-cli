package internal

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReviewFollowupCandidatesFailsClosedWhenProviderHasUntrackedMessages(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	gws.InboxMessages = followupProviderThread(now)

	result, err := ReviewFollowupCandidates(FollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, MaxFollowups: 0, Limit: 20, GWS: gws,
	})
	if err == nil || !strings.Contains(err.Error(), "reconciliation required") {
		t.Fatalf("expected fail-closed reconciliation error, got result=%+v err=%v", result, err)
	}
	if result == nil || result.Audit == nil || result.Audit.Missing != 1 {
		t.Fatalf("expected one reported missing provider message, got %+v", result)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("must not return stale candidates, got %+v", result.Candidates)
	}
	var stored int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM email_messages WHERE campaign_id = ? AND lead_id = ?`, campaignID, leadID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("read-only candidate review mutated messages: %d", stored)
	}
}

func TestReviewFollowupCandidatesReconcilesThenReturnsVerifiedCandidate(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	gws.InboxMessages = followupProviderThread(now)

	result, err := ReviewFollowupCandidates(FollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, MaxFollowups: 0, Limit: 20,
		Reconcile: true, IncludeThread: true, GWS: gws,
	})
	if err != nil {
		t.Fatalf("ReviewFollowupCandidates error: %v", err)
	}
	if result.Reconciliation == nil || result.Reconciliation.Applied != 1 || result.Reconciliation.Remaining != 0 {
		t.Fatalf("unexpected reconciliation: %+v", result.Reconciliation)
	}
	if result.Audit == nil || result.Audit.Missing != 0 {
		t.Fatalf("expected clean verification audit, got %+v", result.Audit)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected one candidate, got %+v", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.CampaignID != campaignID || candidate.LeadID != leadID {
		t.Fatalf("unexpected candidate identity: %+v", candidate)
	}
	if candidate.ToEmail != "lead@example.net" || candidate.FromEmail != "sender@example.com" {
		t.Fatalf("unexpected candidate participants: %+v", candidate)
	}
	if candidate.ReplyCount != 1 || candidate.FollowupCount != 0 {
		t.Fatalf("unexpected candidate counts: %+v", candidate)
	}
	if candidate.LastInboundBody != "Interested" || candidate.LastOutboundBody != "Here are the details" {
		t.Fatalf("unexpected candidate context: %+v", candidate)
	}
	if len(candidate.Thread) != 3 {
		t.Fatalf("expected complete three-message thread, got %+v", candidate.Thread)
	}
}

func TestReviewFollowupCandidatesBlocksOnProviderAccountError(t *testing.T) {
	gws, _, store, _, _ := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	gws.ThreadError = errors.New("provider unavailable")

	result, err := ReviewFollowupCandidates(FollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, GWS: gws,
	})
	if err == nil || !strings.Contains(err.Error(), "provider audit failed") {
		t.Fatalf("expected provider audit failure, got result=%+v err=%v", result, err)
	}
	if result == nil || len(result.Candidates) != 0 {
		t.Fatalf("provider error must return no candidates: %+v", result)
	}
}

func TestFindFollowupCandidatesExcludesPriorRevivalByDefault(t *testing.T) {
	_, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: "<answer@example.com>", ThreadID: "thread-1", InReplyTo: "<reply@example.net>",
		FromEmail: "sender@example.com", ToEmails: "lead@example.net", Subject: "Re: Question",
		TextBody: "Here are the details", OccurredAt: now.AddDate(0, 0, -12),
	})
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: "<revival@example.com>", ThreadID: "thread-1", InReplyTo: "<answer@example.com>",
		FromEmail: "sender@example.com", ToEmails: "lead@example.net", Subject: "Re: Question",
		TextBody: "Did you get a chance to review it?", OccurredAt: now.AddDate(0, 0, -8),
	})

	result, err := FindFollowupCandidates(FindFollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, MaxFollowups: 0, Limit: 20,
	})
	if err != nil {
		t.Fatalf("FindFollowupCandidates error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected prior revival to be excluded, got %+v", result)
	}

	result, err = FindFollowupCandidates(FindFollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, MaxFollowups: 1, Limit: 20,
	})
	if err != nil {
		t.Fatalf("FindFollowupCandidates with override error: %v", err)
	}
	if len(result) != 1 || result[0].FollowupCount != 1 {
		t.Fatalf("expected one explicitly allowed prior revival, got %+v", result)
	}
}

func TestFindFollowupCandidatesExcludesSuppressedLead(t *testing.T) {
	_, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: "<answer@example.com>", ThreadID: "thread-1", InReplyTo: "<reply@example.net>",
		FromEmail: "sender@example.com", ToEmails: "lead@example.net", Subject: "Re: Question",
		TextBody: "Here are the details", OccurredAt: now.AddDate(0, 0, -12),
	})
	if _, err := store.DB.Exec(`INSERT INTO events (
		campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp
	) VALUES (?, ?, 1, 'unsubscribe', 0, '<unsub@example.net>', 'thread-1', ?)`, campaignID, leadID, now.AddDate(0, 0, -10)); err != nil {
		t.Fatal(err)
	}

	result, err := FindFollowupCandidates(FindFollowupCandidatesConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: now.AddDate(0, 0, -30),
		Now: now, MinAge: 7 * 24 * time.Hour, MaxFollowups: 0, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected suppressed lead to be excluded, got %+v", result)
	}
}

func followupProviderThread(now time.Time) []GWSMessage {
	return []GWSMessage{
		{ID: "gmail-root", ThreadID: "thread-1", From: "sender@example.com", To: "lead@example.net", Subject: "Question", TextBody: "Initial", Date: now.AddDate(0, 0, -20), Headers: map[string]string{"Message-ID": "<root@example.com>"}},
		{ID: "gmail-reply", ThreadID: "thread-1", From: "lead@example.net", To: "sender@example.com", Subject: "Re: Question", TextBody: "Interested", InReplyTo: "<root@example.com>", Date: now.AddDate(0, 0, -19), Headers: map[string]string{"Message-ID": "<reply@example.net>", "References": "<root@example.com>"}},
		{ID: "gmail-answer", ThreadID: "thread-1", From: "sender@example.com", To: "lead@example.net", Subject: "Re: Question", TextBody: "Here are the details", InReplyTo: "<reply@example.net>", Date: now.AddDate(0, 0, -12), Headers: map[string]string{"Message-ID": "<answer@example.com>", "References": "<root@example.com> <reply@example.net>"}},
	}
}

func insertFollowupTestMessage(t *testing.T, store *Store, message EmailMessage) {
	t.Helper()
	if err := insertEmailMessage(store.DB, message); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`INSERT INTO events (
		campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp
	) VALUES (?, ?, ?, ?, 0, ?, ?, ?)`, message.CampaignID, message.LeadID, message.AccountID,
		message.Type, message.MessageID, message.ThreadID, message.OccurredAt); err != nil {
		t.Fatal(err)
	}
}
