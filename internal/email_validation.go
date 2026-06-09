package internal

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

const (
	EmailValidationPass         = "pass"
	EmailValidationManualReview = "manual_review"
	EmailValidationFail         = "fail"

	RecipientStatusVerified = "verified"
	RecipientStatusRejected = "rejected"
	RecipientStatusCatchAll = "catch_all"
	RecipientStatusUnknown  = "unknown"
	RecipientStatusNoMX     = "no_mx"
	RecipientStatusFreeMail = "free_email"
)

var smtpSkippedRecipientDomains = map[string]bool{
	"gmail.com":      true,
	"googlemail.com": true,
	"outlook.com":    true,
	"hotmail.com":    true,
	"live.com":       true,
	"msn.com":        true,
	"yahoo.com":      true,
	"yahoo.co.uk":    true,
	"ymail.com":      true,
	"aol.com":        true,
	"icloud.com":     true,
	"me.com":         true,
	"mac.com":        true,
	"protonmail.com": true,
	"proton.me":      true,
}

type EmailValidationPolicy struct {
	AllowFreeEmail bool
	AllowCatchAll  bool
	AllowUnknown   bool
	Timeout        time.Duration
	HeloName       string
	MailFrom       string
}

type LeadEmailValidationRow struct {
	Email            string `json:"email"`
	Domain           string `json:"domain"`
	ValidationStatus string `json:"validation_status"`
	SMTPStatus       string `json:"smtp_status"`
	Detail           string `json:"detail"`
}

type LeadEmailValidationResult struct {
	Rows         []LeadEmailValidationRow `json:"rows"`
	Checked      int                      `json:"checked"`
	Pass         int                      `json:"pass"`
	ManualReview int                      `json:"manual_review"`
	Fail         int                      `json:"fail"`
}

func (r LeadEmailValidationResult) HasBlockingRows() bool {
	return r.ManualReview > 0 || r.Fail > 0
}

type RecipientVerifier interface {
	VerifyRecipients(ctx context.Context, domain string, emails []string) (*RecipientVerificationResult, error)
}

type RecipientVerificationResult struct {
	Results map[string]string
	Error   string
}

type SMTPRecipientVerifier struct {
	Timeout  time.Duration
	HeloName string
	MailFrom string
}

func ValidateLeadEmails(records []LeadRecord, verifier RecipientVerifier, policy EmailValidationPolicy) (*LeadEmailValidationResult, error) {
	if policy.Timeout == 0 {
		policy.Timeout = 10 * time.Second
	}
	if policy.HeloName == "" {
		policy.HeloName = "verify.cold-cli.local"
	}
	if policy.MailFrom == "" {
		policy.MailFrom = "verify@cold-cli.local"
	}
	if verifier == nil {
		verifier = SMTPRecipientVerifier{
			Timeout:  policy.Timeout,
			HeloName: policy.HeloName,
			MailFrom: policy.MailFrom,
		}
	}

	result := &LeadEmailValidationResult{}
	pendingByDomain := map[string][]string{}
	emailOrder := []string{}
	presetRows := map[string]LeadEmailValidationRow{}

	for _, record := range records {
		email := strings.ToLower(strings.TrimSpace(record.Fields["email"]))
		domain := ExtractDomain(email)
		emailOrder = append(emailOrder, email)

		if email == "" {
			presetRows[email] = validationRow(email, "", RecipientStatusUnknown, EmailValidationFail, "missing email")
			continue
		}
		if _, err := mail.ParseAddress(email); err != nil {
			presetRows[email] = validationRow(email, domain, RecipientStatusUnknown, EmailValidationFail, "invalid email syntax")
			continue
		}
		if smtpSkippedRecipientDomains[domain] {
			status, detail := applyRecipientPolicy(RecipientStatusFreeMail, policy)
			presetRows[email] = validationRow(email, domain, RecipientStatusFreeMail, status, detail)
			continue
		}
		pendingByDomain[domain] = append(pendingByDomain[domain], email)
	}

	verifiedRows := map[string]LeadEmailValidationRow{}
	ctx := context.Background()
	for domain, emails := range pendingByDomain {
		uniqueEmails := uniqueEmailValidationStrings(emails)
		verification, err := verifier.VerifyRecipients(ctx, domain, uniqueEmails)
		if err != nil {
			verification = &RecipientVerificationResult{
				Results: map[string]string{},
				Error:   err.Error(),
			}
		}
		if verification.Results == nil {
			verification.Results = map[string]string{}
		}
		for _, email := range uniqueEmails {
			recipientStatus := verification.Results[email]
			if recipientStatus == "" {
				recipientStatus = RecipientStatusUnknown
			}
			status, detail := applyRecipientPolicy(recipientStatus, policy)
			if verification.Error != "" && recipientStatus == RecipientStatusUnknown {
				detail = detail + ": " + verification.Error
			}
			verifiedRows[email] = validationRow(email, domain, recipientStatus, status, detail)
		}
	}

	for _, email := range emailOrder {
		row, ok := presetRows[email]
		if !ok {
			row = verifiedRows[email]
		}
		result.Rows = append(result.Rows, row)
		result.Checked++
		switch row.ValidationStatus {
		case EmailValidationPass:
			result.Pass++
		case EmailValidationManualReview:
			result.ManualReview++
		case EmailValidationFail:
			result.Fail++
		}
	}

	return result, nil
}

func applyRecipientPolicy(recipientStatus string, policy EmailValidationPolicy) (string, string) {
	switch recipientStatus {
	case RecipientStatusVerified:
		return EmailValidationPass, "SMTP RCPT accepted recipient"
	case RecipientStatusRejected:
		return EmailValidationFail, "SMTP RCPT rejected recipient"
	case RecipientStatusNoMX:
		return EmailValidationFail, "recipient domain has no MX records"
	case RecipientStatusFreeMail:
		if policy.AllowFreeEmail {
			return EmailValidationPass, "free-mail domain allowed by flag; mailbox not SMTP-verified"
		}
		return EmailValidationManualReview, "free-mail domain is not reliably SMTP-verifiable; require second source or paid verifier"
	case RecipientStatusCatchAll:
		if policy.AllowCatchAll {
			return EmailValidationPass, "catch-all domain allowed by flag; exact mailbox not verified"
		}
		return EmailValidationManualReview, "catch-all domain; exact mailbox not verified"
	default:
		if policy.AllowUnknown {
			return EmailValidationPass, "unknown SMTP result allowed by flag"
		}
		return EmailValidationManualReview, "unknown SMTP result; require second source or retry"
	}
}

func validationRow(email, domain, smtpStatus, validationStatus, detail string) LeadEmailValidationRow {
	return LeadEmailValidationRow{
		Email:            email,
		Domain:           domain,
		ValidationStatus: validationStatus,
		SMTPStatus:       smtpStatus,
		Detail:           detail,
	}
}

func uniqueEmailValidationStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (v SMTPRecipientVerifier) VerifyRecipients(ctx context.Context, domain string, emails []string) (*RecipientVerificationResult, error) {
	result := &RecipientVerificationResult{Results: map[string]string{}}
	if len(emails) == 0 {
		return result, nil
	}
	timeout := v.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	heloName := v.HeloName
	if heloName == "" {
		heloName = "verify.cold-cli.local"
	}
	mailFrom := v.MailFrom
	if mailFrom == "" {
		mailFrom = "verify@cold-cli.local"
	}

	mxRecords, err := net.LookupMX(domain)
	if err != nil || len(mxRecords) == 0 {
		for _, email := range emails {
			result.Results[email] = RecipientStatusNoMX
		}
		result.Error = "no MX records"
		return result, nil
	}
	sort.Slice(mxRecords, func(i, j int) bool {
		return mxRecords[i].Pref < mxRecords[j].Pref
	})

	host := strings.TrimSuffix(mxRecords[0].Host, ".")
	catchAllEmail := fmt.Sprintf("cold-cli-check-%d-%d@%s", time.Now().UnixNano(), rand.Intn(100000), domain)
	catchAllStatus, err := smtpRCPTStatus(ctx, host, catchAllEmail, heloName, mailFrom, timeout)
	if err == nil && catchAllStatus == 250 {
		for _, email := range emails {
			result.Results[email] = RecipientStatusCatchAll
		}
		return result, nil
	}

	for _, email := range emails {
		code, err := smtpRCPTStatus(ctx, host, email, heloName, mailFrom, timeout)
		if err != nil {
			result.Results[email] = RecipientStatusUnknown
			result.Error = err.Error()
			continue
		}
		switch {
		case code == 250:
			result.Results[email] = RecipientStatusVerified
		case code >= 550 && code <= 553:
			result.Results[email] = RecipientStatusRejected
		default:
			result.Results[email] = RecipientStatusUnknown
			result.Error = fmt.Sprintf("SMTP code %d", code)
		}
	}

	return result, nil
}

func smtpRCPTStatus(ctx context.Context, host, recipient, heloName, mailFrom string, timeout time.Duration) (int, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "25"))
	if err != nil {
		return 0, fmt.Errorf("connecting to MX %s: %w", host, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return 0, fmt.Errorf("opening SMTP session: %w", err)
	}
	defer client.Close()

	if err := client.Hello(heloName); err != nil {
		return 0, fmt.Errorf("SMTP EHLO: %w", err)
	}
	if err := client.Mail(mailFrom); err != nil {
		return 0, fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	if err := client.Rcpt(recipient); err != nil {
		var smtpErr *textproto.Error
		if errors.As(err, &smtpErr) {
			return smtpErr.Code, nil
		}
		return 0, fmt.Errorf("SMTP RCPT TO %s: %w", recipient, err)
	}
	return 250, nil
}
