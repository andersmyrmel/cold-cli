package internal

import (
	"strings"
	"testing"
	"time"
)

type staticSecretResolver map[string]string

func (r staticSecretResolver) ResolveSecret(ref string) (string, error) {
	value, ok := r[ref]
	if !ok {
		return "", &missingSecretError{ref: ref}
	}
	return value, nil
}

type missingSecretError struct {
	ref string
}

func (e *missingSecretError) Error() string {
	return "missing secret " + e.ref
}

func TestSMTPTransport_SendEmail(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	var deliveredAccount Account
	var deliveredPassword string
	var deliveredRecipients []string
	var deliveredMessage string

	transport := NewSMTPTransport(staticSecretResolver{
		"env:SMTP_PASSWORD": "smtp-secret",
	})
	transport.Now = func() time.Time { return now }
	transport.send = func(account Account, password string, recipients []string, msg []byte) error {
		deliveredAccount = account
		deliveredPassword = password
		deliveredRecipients = recipients
		deliveredMessage = string(msg)
		return nil
	}

	messageID, threadID, err := transport.SendEmail(Account{
		Email:           "sender@example.com",
		Provider:        AccountProviderSMTPIMAP,
		SMTPHost:        "smtp.example.com",
		SMTPPort:        587,
		SMTPUsername:    "sender@example.com",
		SMTPPasswordRef: "env:SMTP_PASSWORD",
		SMTPTLSMode:     "starttls",
		IMAPHost:        "imap.example.com",
		IMAPPort:        993,
		IMAPUsername:    "sender@example.com",
		IMAPPasswordRef: "env:SMTP_PASSWORD",
		IMAPTLSMode:     "ssl",
	}, EmailParams{
		FromEmail: "sender@example.com",
		ToEmail:   "lead@example.com",
		Subject:   "Hello",
		Body:      "Hi there",
	})
	if err != nil {
		t.Fatalf("SendEmail error: %v", err)
	}
	if messageID == "" {
		t.Fatal("expected generated message ID")
	}
	if threadID != messageID {
		t.Errorf("expected thread ID to default to message ID, got %s", threadID)
	}
	if deliveredAccount.Email != "sender@example.com" {
		t.Errorf("expected delivered account, got %#v", deliveredAccount)
	}
	if deliveredPassword != "smtp-secret" {
		t.Errorf("expected resolved smtp secret, got %q", deliveredPassword)
	}
	if len(deliveredRecipients) != 1 || deliveredRecipients[0] != "lead@example.com" {
		t.Errorf("unexpected recipients: %#v", deliveredRecipients)
	}
	for _, expected := range []string{
		"Date: Wed, 29 Apr 2026 12:00:00 +0000",
		"Message-ID: " + messageID,
		"From: sender@example.com",
		"To: lead@example.com",
		"Subject: Hello",
		"<div>Hi there</div>",
	} {
		if !strings.Contains(deliveredMessage, expected) {
			t.Errorf("expected delivered message to contain %q\nmessage:\n%s", expected, deliveredMessage)
		}
	}
}

func TestSMTPTransport_PreservesThreadID(t *testing.T) {
	transport := NewSMTPTransport(staticSecretResolver{"env:SMTP_PASSWORD": "smtp-secret"})
	transport.send = func(Account, string, []string, []byte) error { return nil }

	messageID, threadID, err := transport.SendEmail(Account{
		Email:           "sender@example.com",
		Provider:        AccountProviderSMTPIMAP,
		SMTPHost:        "smtp.example.com",
		SMTPPort:        587,
		SMTPUsername:    "sender@example.com",
		SMTPPasswordRef: "env:SMTP_PASSWORD",
		SMTPTLSMode:     "starttls",
	}, EmailParams{
		FromEmail: "sender@example.com",
		ToEmail:   "lead@example.com",
		Subject:   "Re: Hello",
		Body:      "Following up",
		ThreadID:  "<original-thread@example.com>",
	})
	if err != nil {
		t.Fatalf("SendEmail error: %v", err)
	}
	if messageID == "" {
		t.Fatal("expected message ID")
	}
	if threadID != "<original-thread@example.com>" {
		t.Errorf("expected original thread ID, got %s", threadID)
	}
}

func TestSMTPTransport_RequiresSMTPIMAPProvider(t *testing.T) {
	transport := NewSMTPTransport(staticSecretResolver{"env:SMTP_PASSWORD": "smtp-secret"})
	_, _, err := transport.SendEmail(Account{
		Email:    "sender@example.com",
		Provider: AccountProviderGWS,
	}, EmailParams{ToEmail: "lead@example.com"})
	if err == nil {
		t.Fatal("expected provider error")
	}
}
