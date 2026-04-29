package internal

import (
	"fmt"
	"testing"
)

type fakeSMTPVerifier struct {
	err   error
	calls int
}

func (f *fakeSMTPVerifier) VerifyAccount(Account) error {
	f.calls++
	return f.err
}

type fakeIMAPVerifier struct {
	err   error
	calls int
}

func (f *fakeIMAPVerifier) VerifyAccount(Account) error {
	f.calls++
	return f.err
}

func TestVerifySMTPIMAPAccount(t *testing.T) {
	smtp := &fakeSMTPVerifier{}
	imap := &fakeIMAPVerifier{}

	result, err := VerifySMTPIMAPAccount(Account{
		Email:    "sender@example.com",
		Provider: AccountProviderSMTPIMAP,
	}, smtp, imap)
	if err != nil {
		t.Fatalf("VerifySMTPIMAPAccount error: %v", err)
	}
	if !result.SMTPOK || !result.IMAPOK {
		t.Fatalf("expected both checks ok, got %#v", result)
	}
	if smtp.calls != 1 || imap.calls != 1 {
		t.Fatalf("expected both verifiers called once, got smtp=%d imap=%d", smtp.calls, imap.calls)
	}
}

func TestVerifySMTPIMAPAccountFailure(t *testing.T) {
	smtp := &fakeSMTPVerifier{err: fmt.Errorf("smtp failed")}
	imap := &fakeIMAPVerifier{}

	result, err := VerifySMTPIMAPAccount(Account{
		Email:    "sender@example.com",
		Provider: AccountProviderSMTPIMAP,
	}, smtp, imap)
	if err == nil {
		t.Fatal("expected verification error")
	}
	if result.SMTPOK {
		t.Fatal("expected smtp check to fail")
	}
	if !result.IMAPOK {
		t.Fatal("expected imap check to pass")
	}
	if result.SMTPError != "smtp failed" {
		t.Fatalf("expected smtp error retained, got %q", result.SMTPError)
	}
}

func TestVerifySMTPIMAPAccountProviderMismatch(t *testing.T) {
	_, err := VerifySMTPIMAPAccount(Account{
		Email:    "sender@example.com",
		Provider: AccountProviderGWS,
	}, &fakeSMTPVerifier{}, &fakeIMAPVerifier{})
	if err == nil {
		t.Fatal("expected provider mismatch error")
	}
}
