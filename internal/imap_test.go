package internal

import (
	"strings"
	"testing"
	"time"

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
		"Date: Thu, 30 Apr 2026 19:25:43 +0800",
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
	expectedDate := time.Date(2026, time.April, 30, 11, 25, 43, 0, time.UTC)
	if !msg.Date.Equal(expectedDate) {
		t.Errorf("expected parsed date %s, got %s", expectedDate.Format(time.RFC3339), msg.Date.Format(time.RFC3339))
	}
	if msg.Headers["X-Failed-Recipients"] != "failed@example.com" {
		t.Errorf("expected failed recipient header, got %q", msg.Headers["X-Failed-Recipients"])
	}
	if !strings.Contains(msg.Snippet, "Please stop emailing me") {
		t.Errorf("expected body snippet, got %q", msg.Snippet)
	}
}

func TestParseIMAPRawMessageEnvelopeFallback(t *testing.T) {
	envelopeDate := time.Date(2026, time.April, 30, 11, 33, 47, 0, time.UTC)
	msg := ParseIMAPRawMessage("sender@example.com", "INBOX", 7, []byte("not a valid RFC message"), &imap.Envelope{
		MessageId: "<envelope@example.com>",
		Subject:   "Envelope subject",
		InReplyTo: "<sent@example.com>",
		Date:      envelopeDate,
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
	if !msg.Date.Equal(envelopeDate) {
		t.Errorf("expected envelope date %s, got %s", envelopeDate.Format(time.RFC3339), msg.Date.Format(time.RFC3339))
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

func TestIMAPTransportAppendSent(t *testing.T) {
	now := time.Date(2026, time.August, 13, 8, 30, 0, 0, time.UTC)
	transport := NewIMAPTransport(staticSecretResolver{"env:MAIL_PASSWORD": "secret"})
	var gotMailbox string
	var gotPassword string
	var gotMessage string
	transport.appendMessage = func(_ Account, password, mailbox string, flags []string, date time.Time, msg []byte) error {
		gotMailbox = mailbox
		gotPassword = password
		gotMessage = string(msg)
		if len(flags) != 1 || flags[0] != imap.SeenFlag {
			t.Fatalf("unexpected flags: %#v", flags)
		}
		if !date.Equal(now) {
			t.Fatalf("unexpected append date: %s", date)
		}
		return nil
	}

	mailbox, err := transport.AppendSent(Account{
		Email: "sender@example.com", Provider: AccountProviderSMTPIMAP,
		IMAPPasswordRef: "env:MAIL_PASSWORD",
	}, EmailParams{
		FromEmail: "sender@example.com", ToEmail: "lead@example.com",
		Subject: "Re: Hello", Body: "Reply", MessageID: "<reply@example.com>",
		InReplyTo: "<parent@example.com>", References: "<root@example.com> <parent@example.com>", Date: now,
	})
	if err != nil {
		t.Fatalf("AppendSent error: %v", err)
	}
	if mailbox != "Sent" || gotMailbox != "Sent" {
		t.Fatalf("expected Sent mailbox, result=%q called=%q", mailbox, gotMailbox)
	}
	if gotPassword != "secret" {
		t.Fatalf("expected resolved password")
	}
	for _, expected := range []string{
		"Message-ID: <reply@example.com>",
		"In-Reply-To: <parent@example.com>",
		"References: <root@example.com> <parent@example.com>",
	} {
		if !strings.Contains(gotMessage, expected) {
			t.Fatalf("Sent copy missing %q:\n%s", expected, gotMessage)
		}
	}
}
