package internal

import (
	"fmt"
	"os"
	"strings"
)

const secretRefEnvScheme = "env"

// SecretResolver resolves stored secret references into runtime secret values.
type SecretResolver interface {
	ResolveSecret(ref string) (string, error)
}

// EnvSecretResolver resolves env:NAME references from the process environment.
type EnvSecretResolver struct{}

func (EnvSecretResolver) ResolveSecret(ref string) (string, error) {
	return ResolveSecretRef(ref)
}

// ValidateSecretRef verifies that a secret reference uses a supported scheme.
func ValidateSecretRef(ref string) error {
	scheme, target, err := parseSecretRef(ref)
	if err != nil {
		return err
	}
	switch scheme {
	case secretRefEnvScheme:
		if target == "" {
			return fmt.Errorf("env secret reference must include a variable name")
		}
		return nil
	default:
		return fmt.Errorf("unsupported secret reference scheme %q; use env:NAME", scheme)
	}
}

// ResolveSecretRef resolves a secret reference without exposing the value in errors.
func ResolveSecretRef(ref string) (string, error) {
	scheme, target, err := parseSecretRef(ref)
	if err != nil {
		return "", err
	}
	switch scheme {
	case secretRefEnvScheme:
		if target == "" {
			return "", fmt.Errorf("env secret reference must include a variable name")
		}
		value, ok := os.LookupEnv(target)
		if !ok {
			return "", fmt.Errorf("secret environment variable %s is not set", target)
		}
		if value == "" {
			return "", fmt.Errorf("secret environment variable %s is empty", target)
		}
		return value, nil
	default:
		return "", fmt.Errorf("unsupported secret reference scheme %q; use env:NAME", scheme)
	}
}

type SMTPIMAPAccountSecrets struct {
	SMTPPassword string
	IMAPPassword string
}

// ResolveSMTPIMAPAccountSecrets resolves the password references for an SMTP/IMAP account.
func ResolveSMTPIMAPAccountSecrets(account Account, resolver SecretResolver) (*SMTPIMAPAccountSecrets, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return nil, fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}

	smtpPassword, err := resolver.ResolveSecret(account.SMTPPasswordRef)
	if err != nil {
		return nil, fmt.Errorf("resolving SMTP password for %s: %w", account.Email, err)
	}

	imapRef := strings.TrimSpace(account.IMAPPasswordRef)
	if imapRef == "" {
		imapRef = account.SMTPPasswordRef
	}
	imapPassword := smtpPassword
	if imapRef != account.SMTPPasswordRef {
		imapPassword, err = resolver.ResolveSecret(imapRef)
		if err != nil {
			return nil, fmt.Errorf("resolving IMAP password for %s: %w", account.Email, err)
		}
	}

	return &SMTPIMAPAccountSecrets{
		SMTPPassword: smtpPassword,
		IMAPPassword: imapPassword,
	}, nil
}

func parseSecretRef(ref string) (scheme string, target string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("secret reference is required")
	}
	scheme, target, ok := strings.Cut(ref, ":")
	if !ok {
		return "", "", fmt.Errorf("secret reference must use scheme:value format; use env:NAME")
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	target = strings.TrimSpace(target)
	if scheme == "" {
		return "", "", fmt.Errorf("secret reference scheme is required")
	}
	return scheme, target, nil
}
