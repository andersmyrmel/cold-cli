package internal

import (
	"fmt"
	"net/mail"
	"strings"
)

// SendSMTPTestEmailOpts holds a one-off SMTP test email request.
type SendSMTPTestEmailOpts struct {
	RecipientEmail string `json:"recipient_email"`
	Subject        string `json:"subject"`
	Body           string `json:"body"`
}

// SendSMTPTestEmailResult is returned after a test email is accepted by SMTP.
type SendSMTPTestEmailResult struct {
	Email          string `json:"email"`
	RecipientEmail string `json:"recipient_email"`
	MessageID      string `json:"message_id"`
	ThreadID       string `json:"thread_id"`
}

// SendSMTPTestEmail sends a one-off test email through an SMTP/IMAP account.
func SendSMTPTestEmail(account Account, opts SendSMTPTestEmailOpts, resolver SecretResolver) (*SendSMTPTestEmailResult, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return nil, fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	if account.Status == "removed" {
		return nil, fmt.Errorf("account %s has been removed — re-add it first", account.Email)
	}
	if account.Status != "active" {
		return nil, fmt.Errorf("account %s is %s", account.Email, account.Status)
	}

	recipient, err := normalizeTestRecipient(opts.RecipientEmail)
	if err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(opts.Subject)
	if subject == "" {
		subject = "cold-cli test email"
	}
	body := strings.TrimSpace(opts.Body)
	if body == "" {
		body = "This is a test email from cold-cli."
	}

	messageID, threadID, err := NewSMTPTransport(resolver).SendEmail(account, EmailParams{
		FromEmail: account.Email,
		ToEmail:   recipient,
		Subject:   subject,
		Body:      body,
	})
	if err != nil {
		return nil, err
	}

	return &SendSMTPTestEmailResult{
		Email:          account.Email,
		RecipientEmail: recipient,
		MessageID:      messageID,
		ThreadID:       threadID,
	}, nil
}

func normalizeTestRecipient(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("recipient email is required")
	}

	address, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("recipient email is invalid")
	}
	return address.Address, nil
}
