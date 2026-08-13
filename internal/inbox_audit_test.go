package internal

import (
	"fmt"
	"testing"
	"time"
)

type auditThreadIMAPMock struct {
	messages []GWSMessage
	calls    [][]string
}

func (m *auditThreadIMAPMock) ListMessages(Account, time.Time, bool) ([]GWSMessage, error) {
	return nil, fmt.Errorf("broad mailbox listing must not be used by audit")
}

func (m *auditThreadIMAPMock) ListThreadMessages(_ Account, _ time.Time, anchors []string) ([]GWSMessage, error) {
	m.calls = append(m.calls, append([]string(nil), anchors...))
	var matched []GWSMessage
	for _, msg := range m.messages {
		for _, anchor := range anchors {
			if imapMessageBelongsToThread(msg, "", map[string]struct{}{canonicalMessageID(anchor): {}}) {
				matched = append(matched, msg)
				break
			}
		}
	}
	return matched, nil
}

func TestAuditInboxHistoryReportsUntrackedInboundAndOutboundWithoutWriting(t *testing.T) {
	gws, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderGWS)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	gws.InboxMessages = []GWSMessage{
		{ID: "gmail-known", ThreadID: "thread-1", From: "lead@example.net", To: "sender@example.com", InReplyTo: "<root@example.com>", Date: since.Add(48 * time.Hour), Headers: map[string]string{"Message-ID": "<reply@example.net>", "References": "<root@example.com>"}},
		{ID: "gmail-manual", ThreadID: "thread-1", From: "sender@example.com", To: "lead@example.net", InReplyTo: "<reply@example.net>", Date: since.Add(72 * time.Hour), Headers: map[string]string{"Message-ID": "<manual@example.com>", "References": "<root@example.com> <reply@example.net>"}},
		{ID: "gmail-reply-2", ThreadID: "thread-1", From: "lead@example.net", To: "sender@example.com", InReplyTo: "<manual@example.com>", Date: since.Add(96 * time.Hour), Headers: map[string]string{"Message-ID": "<reply-2@example.net>", "References": "<root@example.com> <reply@example.net> <manual@example.com>"}},
		{ID: "unrelated", ThreadID: "unrelated", From: "other@example.org", To: "sender@example.com", Date: since.Add(120 * time.Hour)},
	}

	result, err := AuditInboxHistory(AuditInboxHistoryConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: since, GWS: gws,
	})
	if err != nil {
		t.Fatalf("AuditInboxHistory error: %v", err)
	}
	if result.Missing != 2 || len(result.Messages) != 2 {
		t.Fatalf("expected two missing messages, got %+v", result)
	}
	if len(gws.ListCalls) != 0 || len(gws.ThreadCalls) != 1 {
		t.Fatalf("expected bounded Gmail thread reads, list=%+v threads=%+v", gws.ListCalls, gws.ThreadCalls)
	}
	if result.Messages[0].CampaignID != campaignID || result.Messages[0].LeadID != leadID || result.Messages[0].Direction != EmailMessageDirectionOutbound {
		t.Fatalf("unexpected outbound audit result: %+v", result.Messages[0])
	}
	if result.Messages[1].Direction != EmailMessageDirectionInbound {
		t.Fatalf("unexpected inbound audit result: %+v", result.Messages[1])
	}
	var count int
	if err := store.DB.QueryRow("SELECT COUNT(*) FROM email_messages").Scan(&count); err != nil {
		t.Fatalf("counting email messages: %v", err)
	}
	if count != 2 {
		t.Fatalf("audit mutated stored messages, got %d", count)
	}
}

func TestAuditInboxHistorySearchesIMAPFromCampaignAnchors(t *testing.T) {
	_, _, store, campaignID, leadID := seedStoredReplyThread(t, AccountProviderSMTPIMAP)
	since := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	imap := &auditThreadIMAPMock{messages: []GWSMessage{
		{ID: "<root@example.com>", From: "sender@example.com", To: "lead@example.net", Date: since.Add(24 * time.Hour), Headers: map[string]string{"Message-ID": "<root@example.com>"}},
		{ID: "<manual@example.com>", From: "sender@example.com", To: "lead@example.net", InReplyTo: "<reply@example.net>", Date: since.Add(72 * time.Hour), Headers: map[string]string{"Message-ID": "<manual@example.com>", "References": "<root@example.com> <reply@example.net>"}},
		{ID: "<reply-2@example.net>", From: "lead@example.net", To: "sender@example.com", InReplyTo: "<manual@example.com>", Date: since.Add(96 * time.Hour), Headers: map[string]string{"Message-ID": "<reply-2@example.net>"}},
	}}
	result, err := AuditInboxHistory(AuditInboxHistoryConfig{
		DB: store.DB, WorkspaceID: "storeinspect", Since: since, IMAP: imap,
	})
	if err != nil {
		t.Fatalf("AuditInboxHistory error: %v", err)
	}
	if len(imap.calls) < 2 {
		t.Fatalf("expected iterative anchor discovery, got calls %+v", imap.calls)
	}
	if result.Missing != 2 || result.Messages[0].CampaignID != campaignID || result.Messages[0].LeadID != leadID {
		t.Fatalf("unexpected result: %+v", result)
	}
}
