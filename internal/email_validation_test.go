package internal

import (
	"context"
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
