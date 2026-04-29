package internal

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"
)

// SMTPEmailSender sends rendered emails through generic SMTP accounts.
type SMTPEmailSender interface {
	SendEmail(account Account, params EmailParams) (messageID string, threadID string, err error)
}

// SMTPAccountVerifier verifies SMTP connectivity and authentication.
type SMTPAccountVerifier interface {
	VerifyAccount(account Account) error
}

// SMTPTransport is the production SMTP sender.
type SMTPTransport struct {
	Resolver SecretResolver
	Timeout  time.Duration
	Now      func() time.Time

	send func(account Account, password string, recipients []string, msg []byte) error
}

func NewSMTPTransport(resolver SecretResolver) *SMTPTransport {
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	return &SMTPTransport{
		Resolver: resolver,
		Timeout:  30 * time.Second,
	}
}

func (s *SMTPTransport) SendEmail(account Account, params EmailParams) (string, string, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return "", "", fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	if params.ToEmail == "" {
		return "", "", fmt.Errorf("recipient is required")
	}

	resolver := s.Resolver
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	smtpPassword, err := resolver.ResolveSecret(account.SMTPPasswordRef)
	if err != nil {
		return "", "", fmt.Errorf("resolving SMTP password for %s: %w", account.Email, err)
	}

	if params.MessageID == "" {
		params.MessageID = GenerateRFCMessageID(account.Email)
	}
	if params.Date.IsZero() {
		now := time.Now
		if s.Now != nil {
			now = s.Now
		}
		params.Date = now().UTC()
	}

	msg := []byte(BuildRFCMessage(params))
	send := s.send
	if send == nil {
		send = func(account Account, password string, recipients []string, msg []byte) error {
			return sendSMTPMessage(account, password, recipients, msg, s.Timeout)
		}
	}
	if err := send(account, smtpPassword, []string{params.ToEmail}, msg); err != nil {
		return "", "", err
	}

	threadID := params.ThreadID
	if threadID == "" {
		threadID = params.MessageID
	}
	return params.MessageID, threadID, nil
}

func (s *SMTPTransport) VerifyAccount(account Account) error {
	if account.Provider != AccountProviderSMTPIMAP {
		return fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}

	resolver := s.Resolver
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	smtpPassword, err := resolver.ResolveSecret(account.SMTPPasswordRef)
	if err != nil {
		return fmt.Errorf("resolving SMTP password for %s: %w", account.Email, err)
	}

	client, err := openAuthenticatedSMTPClient(account, smtpPassword, s.Timeout)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quitting SMTP session: %w", err)
	}
	return nil
}

func sendSMTPMessage(account Account, password string, recipients []string, msg []byte, timeout time.Duration) error {
	client, err := openAuthenticatedSMTPClient(account, password, timeout)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Mail(account.Email); err != nil {
		return fmt.Errorf("setting SMTP sender: %w", err)
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("setting SMTP recipient %s: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("opening SMTP data writer: %w", err)
	}
	if _, err := writer.Write(msg); err != nil {
		_ = writer.Close()
		return fmt.Errorf("writing SMTP message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing SMTP data writer: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quitting SMTP session: %w", err)
	}
	return nil
}

func openAuthenticatedSMTPClient(account Account, password string, timeout time.Duration) (*smtp.Client, error) {
	if account.SMTPHost == "" {
		return nil, fmt.Errorf("smtp host is required")
	}
	if account.SMTPPort < 1 || account.SMTPPort > 65535 {
		return nil, fmt.Errorf("smtp port must be between 1 and 65535")
	}
	if account.SMTPUsername == "" {
		return nil, fmt.Errorf("smtp username is required")
	}
	if password == "" {
		return nil, fmt.Errorf("smtp password is required")
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	addr := net.JoinHostPort(account.SMTPHost, strconv.Itoa(account.SMTPPort))
	dialer := &net.Dialer{Timeout: timeout}

	var conn net.Conn
	var err error
	if account.SMTPTLSMode == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: account.SMTPHost,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to SMTP server: %w", err)
	}

	client, err := smtp.NewClient(conn, account.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("creating SMTP client: %w", err)
	}

	if account.SMTPTLSMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, fmt.Errorf("SMTP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{
			ServerName: account.SMTPHost,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("starting SMTP TLS: %w", err)
		}
	}

	if err := client.Auth(unsafePlainAuth(account.SMTPUsername, password)); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("authenticating SMTP user: %w", err)
	}
	return client, nil
}

type smtpPlainAuth struct {
	username string
	password string
}

func unsafePlainAuth(username, password string) smtp.Auth {
	return smtpPlainAuth{username: username, password: password}
}

func (a smtpPlainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	resp := "\x00" + a.username + "\x00" + a.password
	return "PLAIN", []byte(resp), nil
}

func (a smtpPlainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected SMTP auth challenge")
	}
	return nil, nil
}
