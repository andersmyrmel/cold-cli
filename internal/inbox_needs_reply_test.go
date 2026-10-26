package internal

import (
	"strings"
	"testing"
	"time"
)

func TestReviewNeedsReplyCandidatesReturnsProviderVerifiedLatestInbound(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	provider := followupProviderThread(now)[:2]
	gws.InboxMessages = provider

	result, err := ReviewNeedsReplyCandidates(NeedsReplyCandidatesConfig{
		DB: store.DB, WorkspaceID: "workspace-a", Since: now.AddDate(0, 0, -30),
		Now: now, Limit: 20, IncludeThread: true, GWS: gws,
	})
	if err != nil {
		t.Fatalf("ReviewNeedsReplyCandidates error: %v", err)
	}
	if result.Audit == nil || result.Audit.Missing != 0 {
		t.Fatalf("expected clean provider audit: %+v", result.Audit)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("expected one candidate: %+v", result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.CampaignID != campaignID || candidate.LeadID != leadID {
		t.Fatalf("unexpected candidate identity: %+v", candidate)
	}
	if candidate.FromEmail != "sender@example.com" || candidate.ToEmail != "lead@example.net" {
		t.Fatalf("unexpected reply route: %+v", candidate)
	}
	if candidate.LastInboundBody != "Interested" || len(candidate.Thread) != 2 {
		t.Fatalf("missing full thread context: %+v", candidate)
	}
}

func TestFindNeedsReplyCandidatesExcludesThreadAnsweredAfterInbound(t *testing.T) {
	_, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: "<answer@example.com>", ThreadID: "thread-1", InReplyTo: "<reply@example.net>",
		FromEmail: "sender@example.com", ToEmails: "lead@example.net", Subject: "Re: Question",
		TextBody: "Here are the details", OccurredAt: now.AddDate(0, 0, -12),
	})
	result, err := FindNeedsReplyCandidates(FindNeedsReplyCandidatesConfig{
		DB: store.DB, WorkspaceID: "workspace-a", Since: now.AddDate(0, 0, -30),
		Now: now, Limit: 20,
	})
	if err != nil {
		t.Fatalf("FindNeedsReplyCandidates error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("answered thread must not be listed: %+v", result)
	}
}

func TestFindNeedsReplyCandidatesIncludesConversationAcrossProviderThreadIDs(t *testing.T) {
	_, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: "<answer@example.com>", ThreadID: "provider-thread-2", InReplyTo: "<reply@example.net>",
		FromEmail: "sender@example.com", ToEmails: "lead@example.net", Subject: "Re: Question",
		TextBody: "Here are the details", OccurredAt: now.AddDate(0, 0, -12),
	})
	insertFollowupTestMessage(t, store, EmailMessage{
		CampaignID: campaignID, LeadID: leadID, AccountID: 1,
		Direction: EmailMessageDirectionInbound, Type: EmailMessageTypeReply,
		MessageID: "<latest@example.net>", ThreadID: "provider-thread-3", InReplyTo: "<answer@example.com>",
		FromEmail: "lead@example.net", ToEmails: "sender@example.com", Subject: "Re: Question",
		TextBody: "One more question", OccurredAt: now.AddDate(0, 0, -1),
	})

	result, err := FindNeedsReplyCandidates(FindNeedsReplyCandidatesConfig{
		DB: store.DB, WorkspaceID: "workspace-a", Since: now.AddDate(0, 0, -30),
		Now: now, Limit: 20, IncludeThread: true,
	})
	if err != nil {
		t.Fatalf("FindNeedsReplyCandidates error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected one candidate, got %+v", result)
	}
	candidate := result[0]
	if candidate.MessageCount != 4 || candidate.ReplyCount != 2 || len(candidate.Thread) != 4 {
		t.Fatalf("expected full campaign conversation across provider thread IDs, got %+v", candidate)
	}
	if candidate.PreviousOutboundBody != "Here are the details" {
		t.Fatalf("expected prior outbound context from another provider thread ID, got %+v", candidate)
	}
}

func TestReviewNeedsReplyCandidatesFailsClosedWhenProviderHasUntrackedOutbound(t *testing.T) {
	gws, _, store, _, _ := seedStoredReplyThread(t, AccountProviderGWS)
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	gws.InboxMessages = followupProviderThread(now)

	result, err := ReviewNeedsReplyCandidates(NeedsReplyCandidatesConfig{
		DB: store.DB, WorkspaceID: "workspace-a", Since: now.AddDate(0, 0, -30),
		Now: now, Limit: 20, GWS: gws,
	})
	if err == nil || !strings.Contains(err.Error(), "reconciliation required") {
		t.Fatalf("expected fail-closed reconciliation error, got result=%+v err=%v", result, err)
	}
	if result == nil || result.Audit == nil || result.Audit.Missing != 1 || len(result.Candidates) != 0 {
		t.Fatalf("unexpected fail-closed result: %+v", result)
	}

	result, err = ReviewNeedsReplyCandidates(NeedsReplyCandidatesConfig{
		DB: store.DB, WorkspaceID: "workspace-a", Since: now.AddDate(0, 0, -30),
		Now: now, Limit: 20, Reconcile: true, GWS: gws,
	})
	if err != nil {
		t.Fatalf("reconciled review error: %v", err)
	}
	if result.Reconciliation == nil || result.Reconciliation.Remaining != 0 {
		t.Fatalf("expected verified reconciliation: %+v", result.Reconciliation)
	}
	if len(result.Candidates) != 0 {
		t.Fatalf("provider-confirmed outbound answer should clear the queue: %+v", result.Candidates)
	}
}
