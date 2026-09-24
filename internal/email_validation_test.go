package internal

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
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
	records := []LeadRecord{{Fields: map[string]string{"email": "person@gmail.com"}}}
	result, err := ValidateLeadEmails(records, fakeRecipientVerifier{}, EmailValidationPolicy{})
	if err != nil {
		t.Fatalf("ValidateLeadEmails error: %v", err)
	}

	if result.Pass != 0 || result.ManualReview != 1 || result.Fail != 0 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if result.Rows[0].SMTPStatus != RecipientStatusFreeMail {
		t.Fatalf("expected free_email smtp status, got %q", result.Rows[0].SMTPStatus)
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
