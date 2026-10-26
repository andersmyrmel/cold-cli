package internal

import (
	"strings"
	"testing"
)

func TestPreflightCampaignLeadsBlocksPriorCampaignEmailAndDomain(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO campaigns (workspace_id, name, status, sequence_file)
		VALUES ('workspace-b', 'prior-campaign', 'completed', 'sequence.yml')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO leads (email, domain, global_status)
		VALUES ('known@oldco.example', 'oldco.example', 'active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO campaign_leads (campaign_id, lead_id, status)
		VALUES (1, 1, 'replied')`); err != nil {
		t.Fatal(err)
	}

	records := []LeadRecord{
		{Fields: map[string]string{"email": "fresh@newco.example"}},
		{Fields: map[string]string{"email": "known@oldco.example"}},
		{Fields: map[string]string{"email": "other@oldco.example"}},
	}
	result, err := PreflightCampaignLeads(CampaignPreflightConfig{
		DB:           db,
		WorkspaceID:  "workspace-a",
		HistoryScope: CampaignHistoryScopeAll,
		CheckDomains: true,
		Records:      records,
		EmailVerifier: fakeRecipientVerifier{results: map[string]string{
			"fresh@newco.example": RecipientStatusVerified,
		}},
	})
	if err != nil {
		t.Fatalf("PreflightCampaignLeads error: %v", err)
	}
	if result.Ready != 1 || result.ManualReview != 0 || result.Blocked != 2 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if result.Rows[0].Status != CampaignPreflightReady {
		t.Fatalf("fresh lead should be ready: %+v", result.Rows[0])
	}
	if result.Rows[1].Status != CampaignPreflightBlocked || !containsPreflightReason(result.Rows[1], "prior_email_campaign") {
		t.Fatalf("exact prior email should be blocked: %+v", result.Rows[1])
	}
	if result.Rows[2].Status != CampaignPreflightBlocked || !containsPreflightReason(result.Rows[2], "prior_domain_campaign") {
		t.Fatalf("prior domain should be blocked: %+v", result.Rows[2])
	}
	if len(result.Rows[2].History) != 1 || result.Rows[2].History[0].WorkspaceID != "workspace-b" {
		t.Fatalf("expected cross-workspace history evidence: %+v", result.Rows[2].History)
	}
}

func TestPreflightCampaignLeadsBlocksInputDuplicatesButNotSharedFreeMailDomain(t *testing.T) {
	db := testDB(t)
	records := []LeadRecord{
		{Fields: map[string]string{"email": "one@same.example"}},
		{Fields: map[string]string{"email": "two@same.example"}},
		{Fields: map[string]string{"email": "duplicate@unique.example"}},
		{Fields: map[string]string{"email": "DUPLICATE@unique.example"}},
		{Fields: map[string]string{"email": "first@gmail.com"}},
		{Fields: map[string]string{"email": "second@gmail.com"}},
	}
	result, err := PreflightCampaignLeads(CampaignPreflightConfig{
		DB: db, WorkspaceID: "workspace-a", HistoryScope: CampaignHistoryScopeAll,
		CheckDomains: true, Records: records, SkipEmailValidation: true,
	})
	if err != nil {
		t.Fatalf("PreflightCampaignLeads error: %v", err)
	}
	if result.Ready != 2 || result.Blocked != 4 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if !containsPreflightReason(result.Rows[0], "duplicate_input_domain") || !containsPreflightReason(result.Rows[1], "duplicate_input_domain") {
		t.Fatalf("same company domain should be blocked: %+v %+v", result.Rows[0], result.Rows[1])
	}
	if !containsPreflightReason(result.Rows[2], "duplicate_input_email") || !containsPreflightReason(result.Rows[3], "duplicate_input_email") {
		t.Fatalf("case-insensitive duplicate email should be blocked: %+v %+v", result.Rows[2], result.Rows[3])
	}
	if result.Rows[4].Status != CampaignPreflightReady || result.Rows[5].Status != CampaignPreflightReady {
		t.Fatalf("unrelated free-mail recipients must not collide by domain: %+v %+v", result.Rows[4], result.Rows[5])
	}
}

func TestPreflightCampaignLeadsPreservesEmailManualReviewAndSuppression(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO leads (email, domain, global_status)
		VALUES ('blocked@example.net', 'example.net', 'blacklisted')`); err != nil {
		t.Fatal(err)
	}
	records := []LeadRecord{
		{Fields: map[string]string{"email": "catch@example.com"}},
		{Fields: map[string]string{"email": "blocked@example.net"}},
	}
	result, err := PreflightCampaignLeads(CampaignPreflightConfig{
		DB: db, WorkspaceID: "workspace-a", HistoryScope: CampaignHistoryScopeAll,
		CheckDomains: true, Records: records,
		EmailVerifier: fakeRecipientVerifier{results: map[string]string{
			"catch@example.com": RecipientStatusCatchAll,
		}},
	})
	if err != nil {
		t.Fatalf("PreflightCampaignLeads error: %v", err)
	}
	if result.ManualReview != 1 || result.Blocked != 1 || result.Ready != 0 {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if result.Rows[0].Status != CampaignPreflightManualReview || result.Rows[0].SMTPStatus != RecipientStatusCatchAll {
		t.Fatalf("catch-all should require review: %+v", result.Rows[0])
	}
	if result.Rows[1].Status != CampaignPreflightBlocked || !containsPreflightReason(result.Rows[1], "global_suppression") {
		t.Fatalf("global suppression should block: %+v", result.Rows[1])
	}
	if !result.HasBlockingRows() {
		t.Fatal("expected strict preflight to report blocking rows")
	}

	result, err = PreflightCampaignLeads(CampaignPreflightConfig{
		DB: db, WorkspaceID: "workspace-a", HistoryScope: CampaignHistoryScopeAll,
		CheckDomains: true,
		Records:      []LeadRecord{{Fields: map[string]string{"email": "other@example.net"}}},
		EmailVerifier: fakeRecipientVerifier{results: map[string]string{
			"other@example.net": RecipientStatusVerified,
		}},
	})
	if err != nil {
		t.Fatalf("PreflightCampaignLeads domain-only suppression check error: %v", err)
	}
	if result.Rows[0].Status != CampaignPreflightReady {
		t.Fatalf("an email-level suppression must not suppress every address at the domain: %+v", result.Rows[0])
	}
}

func containsPreflightReason(row CampaignPreflightRow, fragment string) bool {
	for _, reason := range row.Reasons {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}
