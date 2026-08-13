package internal

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	imap "github.com/emersion/go-imap"
)

type fakeMailboxIMAPClient struct {
	mailboxes []*imap.MailboxInfo
	messages  map[string][]byte
	selected  string
}

func (f *fakeMailboxIMAPClient) List(_, _ string, ch chan *imap.MailboxInfo) error {
	defer close(ch)
	for _, mailbox := range f.mailboxes {
		ch <- mailbox
	}
	return nil
}

func (f *fakeMailboxIMAPClient) Select(name string, _ bool) (*imap.MailboxStatus, error) {
	f.selected = name
	return &imap.MailboxStatus{Name: name}, nil
}

func (f *fakeMailboxIMAPClient) UidSearch(*imap.SearchCriteria) ([]uint32, error) {
	if _, ok := f.messages[f.selected]; ok {
		return []uint32{1}, nil
	}
	return nil, nil
}

func (f *fakeMailboxIMAPClient) UidFetch(_ *imap.SeqSet, _ []imap.FetchItem, ch chan *imap.Message) error {
	defer close(ch)
	raw, ok := f.messages[f.selected]
	if !ok {
		return nil
	}
	section := &imap.BodySectionName{}
	ch <- &imap.Message{Uid: 1, Body: map[*imap.BodySectionName]imap.Literal{section: bytes.NewReader(raw)}}
	return nil
}

func (f *fakeMailboxIMAPClient) Logout() error { return nil }

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

func TestParseIMAPRawMessageDecodesMultipartBodies(t *testing.T) {
	raw := strings.Join([]string{
		"From: Lead <lead@example.net>",
		"To: sender@example.com",
		"Subject: Re: details",
		"Message-ID: <multipart@example.net>",
		"In-Reply-To: <root@example.com>",
		"References: <root@example.com>",
		"Date: Mon, 10 Aug 2026 10:00:00 +0000",
		"MIME-Version: 1.0",
		"Content-Type: multipart/alternative; boundary=thread-boundary",
		"",
		"--thread-boundary",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"This is the full reply =E2=80=94 including details.",
		"--thread-boundary",
		"Content-Type: text/html; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"<p>This is the <strong>full reply</strong> =E2=80=94 including details.</p>",
		"--thread-boundary--",
	}, "\r\n")

	msg := ParseIMAPRawMessage("sender@example.com", "INBOX", 42, []byte(raw), nil)
	if msg.TextBody != "This is the full reply — including details." {
		t.Fatalf("unexpected text body: %q", msg.TextBody)
	}
	if msg.HTMLBody != "<p>This is the <strong>full reply</strong> — including details.</p>" {
		t.Fatalf("unexpected HTML body: %q", msg.HTMLBody)
	}
	if msg.Snippet != msg.TextBody {
		t.Fatalf("expected plain-text snippet, got %q", msg.Snippet)
	}
}

func TestParseIMAPRawMessageDecodesWindows1252(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: Lead <lead@example.net>",
		"To: sender@example.com",
		"Subject: Re: details",
		"Message-ID: <windows@example.net>",
		"Content-Type: text/plain; charset=Windows-1252",
		"Content-Transfer-Encoding: 8bit",
		"",
		"",
	}, "\r\n"))
	raw = append(raw, []byte{'I', 0x92, 'm', ' ', 'i', 'n'}...)

	msg := ParseIMAPRawMessage("sender@example.com", "INBOX", 43, raw, nil)
	if msg.TextBody != "I’m in" {
		t.Fatalf("unexpected decoded body: %q", msg.TextBody)
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

func TestIMAPThreadSearchCriteriaMatchesThreadingHeaders(t *testing.T) {
	criteria := imapThreadSearchCriteria(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), []string{
		"<root@example.com>", "<reply@example.net>", "not-an-rfc-id",
	})
	if criteria.Since.IsZero() {
		t.Fatal("expected a since bound")
	}
	if len(criteria.Or) != 1 {
		t.Fatalf("expected one nested OR tree, got %+v", criteria.Or)
	}
}

func TestIMAPAuditSearchCriteriaOmitsMessageIDHeader(t *testing.T) {
	criteria := imapThreadSearchCriteriaWithMessageID(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), []string{
		"<root@example.com>",
	}, false)
	headers := collectIMAPSearchHeaders(criteria)
	if headers["message-id"] {
		t.Fatalf("audit criteria should not search Message-ID: %+v", headers)
	}
	if !headers["references"] || !headers["in-reply-to"] {
		t.Fatalf("audit criteria missing thread headers: %+v", headers)
	}
}

func collectIMAPSearchHeaders(criteria *imap.SearchCriteria) map[string]bool {
	headers := map[string]bool{}
	var walk func(*imap.SearchCriteria)
	walk = func(current *imap.SearchCriteria) {
		if current == nil {
			return
		}
		for name := range current.Header {
			headers[strings.ToLower(name)] = true
		}
		for _, pair := range current.Or {
			walk(pair[0])
			walk(pair[1])
		}
	}
	walk(criteria)
	return headers
}

func TestIMAPSearchRateLimitWait(t *testing.T) {
	wait, ok := imapSearchRateLimitWait(fmt.Errorf("search rate limit exceeded: please wait 54s before trying again"))
	if !ok || wait != 55*time.Second {
		t.Fatalf("unexpected rate limit wait: %s, %t", wait, ok)
	}
	if _, ok := imapSearchRateLimitWait(fmt.Errorf("connection closed")); ok {
		t.Fatal("ordinary errors must not be treated as rate limits")
	}
}

func TestListThreadMessagesSearchesArchiveAndSkipsDrafts(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"Message-ID: <manual@example.com>",
		"From: sender@example.com",
		"To: lead@example.net",
		"References: <root@example.com>",
		"Date: Thu, 13 Aug 2026 09:00:00 +0000",
		"", "Manual follow-up",
	}, "\r\n"))
	fake := &fakeMailboxIMAPClient{
		mailboxes: []*imap.MailboxInfo{
			{Name: "INBOX"},
			{Name: "Archive"},
			{Name: "Drafts", Attributes: []string{"\\Drafts"}},
			{Name: "Containers", Attributes: []string{"\\Noselect"}},
		},
		messages: map[string][]byte{"Archive": raw, "Drafts": raw},
	}
	transport := NewIMAPTransport(staticSecretResolver{"env:MAIL_PASSWORD": "secret"})
	transport.openIMAPClient = func(Account, string) (imapClient, error) { return fake, nil }
	messages, err := transport.ListThreadMessages(Account{
		Email: "sender@example.com", Provider: AccountProviderSMTPIMAP,
		IMAPPasswordRef: "env:MAIL_PASSWORD",
	}, time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC), []string{"<root@example.com>"})
	if err != nil {
		t.Fatalf("ListThreadMessages error: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != "<manual@example.com>" {
		t.Fatalf("expected archived message, got %+v", messages)
	}
}
