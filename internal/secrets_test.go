package internal

import (
	"strings"
	"testing"
)

func TestValidateSecretRef(t *testing.T) {
	if err := ValidateSecretRef("env:MAIL_PASSWORD"); err != nil {
		t.Fatalf("expected env ref to validate: %v", err)
	}
}

func TestValidateSecretRefRejectsRawSecret(t *testing.T) {
	err := ValidateSecretRef("plain-password")
	if err == nil {
		t.Fatal("expected raw secret to be rejected")
	}
	if !strings.Contains(err.Error(), "scheme:value") {
		t.Fatalf("expected scheme guidance, got: %v", err)
	}
}

func TestValidateSecretRefRejectsUnsupportedScheme(t *testing.T) {
	err := ValidateSecretRef("vault:path/to/secret")
	if err == nil {
		t.Fatal("expected unsupported scheme to be rejected")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported scheme error, got: %v", err)
	}
}

func TestResolveSecretRefEnv(t *testing.T) {
	t.Setenv("MAIL_PASSWORD", "secret-value")

	value, err := ResolveSecretRef("env:MAIL_PASSWORD")
	if err != nil {
		t.Fatalf("ResolveSecretRef error: %v", err)
	}
	if value != "secret-value" {
		t.Fatalf("expected secret value, got %q", value)
	}
}

func TestResolveSecretRefMissingEnv(t *testing.T) {
	errVar := "MAIL_PASSWORD_DOES_NOT_EXIST"
	t.Setenv(errVar, "")
	t.Setenv(errVar+"_OTHER", "value")

	_, err := ResolveSecretRef("env:" + errVar)
	if err == nil {
		t.Fatal("expected empty env var to fail")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty env var error, got: %v", err)
	}

	_, err = ResolveSecretRef("env:MAIL_PASSWORD_UNSET")
	if err == nil {
		t.Fatal("expected unset env var to fail")
	}
	if !strings.Contains(err.Error(), "not set") {
		t.Fatalf("expected not set error, got: %v", err)
	}
}

func TestResolveSMTPIMAPAccountSecrets(t *testing.T) {
	t.Setenv("SMTP_PASSWORD", "smtp-secret")
	t.Setenv("IMAP_PASSWORD", "imap-secret")

	secrets, err := ResolveSMTPIMAPAccountSecrets(Account{
		Email:           "sender@example.com",
		Provider:        AccountProviderSMTPIMAP,
		SMTPPasswordRef: "env:SMTP_PASSWORD",
		IMAPPasswordRef: "env:IMAP_PASSWORD",
	}, EnvSecretResolver{})
	if err != nil {
		t.Fatalf("ResolveSMTPIMAPAccountSecrets error: %v", err)
	}
	if secrets.SMTPPassword != "smtp-secret" {
		t.Errorf("expected smtp secret, got %q", secrets.SMTPPassword)
	}
	if secrets.IMAPPassword != "imap-secret" {
		t.Errorf("expected imap secret, got %q", secrets.IMAPPassword)
	}
}

func TestResolveSMTPIMAPAccountSecretsReusesSMTPRef(t *testing.T) {
	t.Setenv("MAIL_PASSWORD", "shared-secret")

	secrets, err := ResolveSMTPIMAPAccountSecrets(Account{
		Email:           "sender@example.com",
		Provider:        AccountProviderSMTPIMAP,
		SMTPPasswordRef: "env:MAIL_PASSWORD",
	}, nil)
	if err != nil {
		t.Fatalf("ResolveSMTPIMAPAccountSecrets error: %v", err)
	}
	if secrets.SMTPPassword != "shared-secret" || secrets.IMAPPassword != "shared-secret" {
		t.Errorf("expected shared secret, got smtp=%q imap=%q", secrets.SMTPPassword, secrets.IMAPPassword)
	}
}
