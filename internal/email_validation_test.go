package internal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

type fakeRecipientVerifier struct {
	results map[string]string
	err     error
}

func (f fakeRecipientVerifier) VerifyRecipients(_ context.Context, _ string, emails []string) (*RecipientVerificationResult, error) {
	out := &RecipientVerificationResult{Results: map[string]string{}}
	for _, email := range emails {
		if status, ok := f.results[email]; ok {
			out.Results[email] = status
		} else {
			out.Results[email] = RecipientStatusUnknown
		}
	}
	return out, f.err
}

func TestValidateLeadEmails_CompanyDomainVerifiedPasses(t *testing.T) {
	records := []LeadRecord{{Fields: map[string]string{"email": "founder@example.com"}}}
	result, err := ValidateLeadEmails(records, fakeRecipientVerifier{
		results: map[string]string{"founder@example.com": RecipientStatusVerified},
	}, EmailValidationPolicy{})
	if err != nil {
		t.Fatalf("ValidateLeadEmails error: %v", err)
	}

	if result.Pass != 1 || result.ManualReview != 0 || result.Fail != 0 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if result.Rows[0].SMTPStatus != RecipientStatusVerified {
		t.Fatalf("expected verified smtp status, got %q", result.Rows[0].SMTPStatus)
	}
}

func TestValidateLeadEmails_FreeMailRequiresManualReviewByDefault(t *testing.T) {
	records := []LeadRecord{{Fields: map[string]string{"email": "person@gmail.com"}}, {Fields: map[string]string{"email": "person@pm.me"}}, {Fields: map[string]string{"email": "person@protonmail.ch"}}}
	result, err := ValidateLeadEmails(records, fakeRecipientVerifier{}, EmailValidationPolicy{})
	if err != nil {
		t.Fatalf("ValidateLeadEmails error: %v", err)
	}

	if result.Pass != 0 || result.ManualReview != 3 || result.Fail != 0 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	for _, row := range result.Rows {
		if row.SMTPStatus != RecipientStatusFreeMail {
			t.Fatalf("expected free_email smtp status, got %q", row.SMTPStatus)
		}
	}
}

func TestValidateLeadEmails_RejectedFailsAndCatchAllNeedsReview(t *testing.T) {
	records := []LeadRecord{
		{Fields: map[string]string{"email": "dead@example.com"}},
		{Fields: map[string]string{"email": "catch@example.com"}},
	}
	result, err := ValidateLeadEmails(records, fakeRecipientVerifier{
		results: map[string]string{
			"dead@example.com":  RecipientStatusRejected,
			"catch@example.com": RecipientStatusCatchAll,
		},
	}, EmailValidationPolicy{})
	if err != nil {
		t.Fatalf("ValidateLeadEmails error: %v", err)
	}

	if result.Pass != 0 || result.ManualReview != 1 || result.Fail != 1 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if !result.HasBlockingRows() {
		t.Fatal("expected blocking rows")
	}
}

func TestValidateLeadEmails_AllowFlagsCanPassRiskyStatuses(t *testing.T) {
	records := []LeadRecord{
		{Fields: map[string]string{"email": "person@gmail.com"}},
		{Fields: map[string]string{"email": "catch@example.com"}},
		{Fields: map[string]string{"email": "unknown@example.com"}},
	}
	result, err := ValidateLeadEmails(records, fakeRecipientVerifier{
		results: map[string]string{
			"catch@example.com":   RecipientStatusCatchAll,
			"unknown@example.com": RecipientStatusUnknown,
		},
	}, EmailValidationPolicy{
		AllowFreeEmail: true,
		AllowCatchAll:  true,
		AllowUnknown:   true,
	})
	if err != nil {
		t.Fatalf("ValidateLeadEmails error: %v", err)
	}

	if result.Pass != 3 || result.ManualReview != 0 || result.Fail != 0 {
		t.Fatalf("unexpected summary: %+v", result)
	}
}

func TestSMTPRecipientVerifier_MXLookupFailureClassification(t *testing.T) {
	tests := []struct {
		name             string
		records          []*net.MX
		lookupErr        error
		recipientStatus  string
		validationStatus string
		errorContains    string
	}{
		{
			name:             "confirmed NXDOMAIN",
			lookupErr:        &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true},
			recipientStatus:  RecipientStatusNoMX,
			validationStatus: EmailValidationFail,
		},
		{
			name:             "successful lookup without MX records",
			records:          []*net.MX{},
			recipientStatus:  RecipientStatusNoMX,
			validationStatus: EmailValidationFail,
		},
		{
			name:             "DNS timeout",
			lookupErr:        &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true},
			recipientStatus:  RecipientStatusUnknown,
			validationStatus: EmailValidationManualReview,
			errorContains:    "i/o timeout",
		},
		{
			name:             "temporary DNS failure",
			lookupErr:        &net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true},
			recipientStatus:  RecipientStatusUnknown,
			validationStatus: EmailValidationManualReview,
			errorContains:    "server misbehaving",
		},
		{
			name:             "temporary failure with misleading not-found flag",
			lookupErr:        &net.DNSError{Err: "temporary resolver failure", Name: "example.com", IsNotFound: true, IsTemporary: true},
			recipientStatus:  RecipientStatusUnknown,
			validationStatus: EmailValidationManualReview,
			errorContains:    "temporary resolver failure",
		},
		{
			name:             "unclassified DNS failure",
			lookupErr:        errors.New("resolver unavailable"),
			recipientStatus:  RecipientStatusUnknown,
			validationStatus: EmailValidationManualReview,
			errorContains:    "resolver unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := SMTPRecipientVerifier{
				lookupMX: func(domain string) ([]*net.MX, error) {
					if domain != "example.com" {
						t.Fatalf("unexpected lookup domain %q", domain)
					}
					return tt.records, tt.lookupErr
				},
			}
			result, err := ValidateLeadEmails(
				[]LeadRecord{{Fields: map[string]string{"email": "founder@example.com"}}},
				verifier,
				EmailValidationPolicy{},
			)
			if err != nil {
				t.Fatalf("ValidateLeadEmails: %v", err)
			}
			if len(result.Rows) != 1 {
				t.Fatalf("expected one result, got %+v", result)
			}
			row := result.Rows[0]
			if row.SMTPStatus != tt.recipientStatus || row.ValidationStatus != tt.validationStatus {
				t.Fatalf("got SMTP %q / validation %q, want %q / %q", row.SMTPStatus, row.ValidationStatus, tt.recipientStatus, tt.validationStatus)
			}
			if tt.errorContains != "" && !strings.Contains(row.Detail, tt.errorContains) {
				t.Fatalf("expected detail to contain %q, got %q", tt.errorContains, row.Detail)
			}
		})
	}
}

func TestSMTPRecipientVerifier_IPv4PreferencePreservesRecipientClassification(t *testing.T) {
	tests := []struct {
		name           string
		fakeCode       int
		recipientCode  int
		wantSMTPStatus string
		wantProbes     int
	}{
		{name: "verified mailbox", fakeCode: 550, recipientCode: 250, wantSMTPStatus: RecipientStatusVerified, wantProbes: 2},
		{name: "catch-all domain", fakeCode: 250, recipientCode: 250, wantSMTPStatus: RecipientStatusCatchAll, wantProbes: 1},
		{name: "rejected mailbox", fakeCode: 550, recipientCode: 550, wantSMTPStatus: RecipientStatusRejected, wantProbes: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var networks []string
			verifier := SMTPRecipientVerifier{
				lookupMX: func(string) ([]*net.MX, error) {
					return []*net.MX{{Host: "mx.example.com.", Pref: 10}}, nil
				},
				dialContext: func(_ context.Context, network, address string) (net.Conn, error) {
					if address != "mx.example.com:25" {
						t.Fatalf("unexpected MX address %q", address)
					}
					networks = append(networks, network)
					return smtpTestConnection(t, tt.fakeCode, tt.recipientCode), nil
				},
			}
			result, err := verifier.VerifyRecipients(context.Background(), "example.com", []string{"founder@example.com"})
			if err != nil {
				t.Fatalf("VerifyRecipients: %v", err)
			}
			if got := result.Results["founder@example.com"]; got != tt.wantSMTPStatus {
				t.Fatalf("SMTP status = %q, want %q (error %q)", got, tt.wantSMTPStatus, result.Error)
			}
			if len(networks) != tt.wantProbes {
				t.Fatalf("got %d probes, want %d", len(networks), tt.wantProbes)
			}
			for _, network := range networks {
				if network != "tcp4" {
					t.Fatalf("dialed %q, want tcp4", network)
				}
			}
		})
	}
}

func TestSMTPRCPTStatus_FallsBackToIPv6OnlyWhenIPv4CannotConnect(t *testing.T) {
	var networks []string
	code, err := smtpRCPTStatus(
		context.Background(), "mx.example.com", "founder@example.com", "verify.cold-cli.local", "verify@cold-cli.local", time.Second,
		func(_ context.Context, network, address string) (net.Conn, error) {
			networks = append(networks, network)
			if network == "tcp4" {
				return nil, errors.New("IPv4 unavailable")
			}
			return smtpTestConnection(t, 550, 250), nil
		},
	)
	if err != nil || code != 250 {
		t.Fatalf("SMTP probe = %d, %v; want 250, nil", code, err)
	}
	if got := strings.Join(networks, ","); got != "tcp4,tcp6" {
		t.Fatalf("dial sequence = %q, want tcp4,tcp6", got)
	}
}

func TestSMTPRCPTStatus_BothIPFamiliesUnavailable(t *testing.T) {
	var networks []string
	_, err := smtpRCPTStatus(
		context.Background(), "mx.example.com", "founder@example.com", "verify.cold-cli.local", "verify@cold-cli.local", time.Second,
		func(_ context.Context, network, _ string) (net.Conn, error) {
			networks = append(networks, network)
			return nil, fmt.Errorf("%s unavailable", network)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "tcp4 unavailable") || !strings.Contains(err.Error(), "tcp6 unavailable") {
		t.Fatalf("expected both connection errors, got %v", err)
	}
	if got := strings.Join(networks, ","); got != "tcp4,tcp6" {
		t.Fatalf("dial sequence = %q, want tcp4,tcp6", got)
	}
}

func smtpTestConnection(t *testing.T, fakeCode, recipientCode int) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		fmt.Fprint(server, "220 mx.example.com ESMTP ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				fmt.Fprint(server, "250 mx.example.com\r\n")
			case strings.HasPrefix(line, "MAIL FROM:"):
				fmt.Fprint(server, "250 sender accepted\r\n")
			case strings.HasPrefix(line, "RCPT TO:"):
				code := recipientCode
				if strings.Contains(line, "cold-cli-check-") {
					code = fakeCode
				}
				fmt.Fprintf(server, "%d recipient response\r\n", code)
			case strings.HasPrefix(line, "QUIT"):
				fmt.Fprint(server, "221 goodbye\r\n")
				return
			default:
				fmt.Fprint(server, "500 unsupported command\r\n")
			}
		}
	}()
	return client
}
