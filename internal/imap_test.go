package internal

import (
	"strings"
	"testing"

	imap "github.com/emersion/go-imap"
)

func TestParseIMAPRawMessage(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"Message-ID: <reply-1@example.com>",
		"From: John <john@acme.com>",
		"To: sender@example.com",
		"Subject: =?utf-8?q?Re=3A_Hello?=",
		"In-Reply-To: <sent-1@example.com>",
		"References: <thread-root@example.com> <sent-1@example.com>",
		"X-Failed-Recipients: failed@example.com",
		"",
		"Please stop emailing me.",
	}, "\r\n"))

	msg := ParseIMAPRawMessage("sender@example.com", "INBOX", 42, raw, nil)
	if msg.ID != "<reply-1@example.com>" {
		t.Errorf("expected message ID, got %s", msg.ID)
	}
	if msg.ThreadID != "<thread-root@example.com>" {
		t.Errorf("expected first reference as thread ID, got %s", msg.ThreadID)
	}
	if msg.InReplyTo != "<sent-1@example.com>" {
		t.Errorf("expected in-reply-to, got %s", msg.InReplyTo)
	}
	if msg.Subject != "Re: Hello" {
		t.Errorf("expected decoded subject, got %q", msg.Subject)
	}
	if msg.Headers["X-Failed-Recipients"] != "failed@example.com" {
		t.Errorf("expected failed recipient header, got %q", msg.Headers["X-Failed-Recipients"])
	}
	if !strings.Contains(msg.Snippet, "Please stop emailing me") {
		t.Errorf("expected body snippet, got %q", msg.Snippet)
	}
}

func TestParseIMAPRawMessageEnvelopeFallback(t *testing.T) {
	msg := ParseIMAPRawMessage("sender@example.com", "INBOX", 7, []byte("not a valid RFC message"), &imap.Envelope{
		MessageId: "<envelope@example.com>",
		Subject:   "Envelope subject",
		InReplyTo: "<sent@example.com>",
	})
	if msg.ID != "<envelope@example.com>" {
		t.Errorf("expected envelope message ID, got %s", msg.ID)
	}
	if msg.Subject != "Envelope subject" {
		t.Errorf("expected envelope subject, got %q", msg.Subject)
	}
	if msg.InReplyTo != "<sent@example.com>" {
		t.Errorf("expected envelope InReplyTo, got %q", msg.InReplyTo)
	}
}

func TestDedupeMailboxMessages(t *testing.T) {
	messages := []GWSMessage{
		{ID: "<a@example.com>", Subject: "one"},
		{ID: "<a@example.com>", Subject: "duplicate"},
		{ID: "<b@example.com>", Subject: "two"},
	}
	deduped := dedupeMailboxMessages(messages)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(deduped))
	}
	if deduped[0].Subject != "one" {
		t.Errorf("expected first message preserved, got %q", deduped[0].Subject)
	}
}
