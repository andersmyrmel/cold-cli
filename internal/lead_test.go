package internal

import (
	"testing"
)

func TestPauseLead_HappyPath(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04', 'pending')")

	result, err := PauseLead(db, "john@acme.com")
	if err != nil {
		t.Fatalf("PauseLead error: %v", err)
	}
	if result.PausedCampaigns != 1 {
		t.Errorf("expected 1 paused campaign, got %d", result.PausedCampaigns)
	}
	if result.CancelledSends != 2 {
		t.Errorf("expected 2 cancelled sends, got %d", result.CancelledSends)
	}

	// Verify campaign_lead status
	var clStatus string
	db.QueryRow("SELECT status FROM campaign_leads WHERE lead_id = 1").Scan(&clStatus)
	if clStatus != "paused" {
		t.Errorf("expected campaign_lead status 'paused', got %q", clStatus)
	}

	// Verify sends cancelled
	var pending int
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE lead_id = 1 AND status = 'pending'").Scan(&pending)
	if pending != 0 {
		t.Errorf("expected 0 pending sends, got %d", pending)
	}
}

func TestPauseLead_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := PauseLead(db, "nonexistent@x.com")
	if err == nil {
		t.Error("expected error for non-existent lead")
	}
}

func TestResumeLead_HappyPath(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file, status) VALUES ('c1', 'seq.yml', 'active')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'paused')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'cancelled')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04', 'cancelled')")

	result, err := ResumeLead(db, "john@acme.com")
	if err != nil {
		t.Fatalf("ResumeLead error: %v", err)
	}
	if result.ResumedCampaigns != 1 {
		t.Errorf("expected 1 resumed campaign, got %d", result.ResumedCampaigns)
	}
	if result.RestoredSends != 2 {
		t.Errorf("expected 2 restored sends, got %d", result.RestoredSends)
	}

	// Verify campaign_lead status
	var clStatus string
	db.QueryRow("SELECT status FROM campaign_leads WHERE lead_id = 1").Scan(&clStatus)
	if clStatus != "active" {
		t.Errorf("expected campaign_lead status 'active', got %q", clStatus)
	}

	// Verify sends restored
	var pending int
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE lead_id = 1 AND status = 'pending'").Scan(&pending)
	if pending != 2 {
		t.Errorf("expected 2 pending sends, got %d", pending)
	}
}

func TestResumeLead_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := ResumeLead(db, "nonexistent@x.com")
	if err == nil {
		t.Error("expected error for non-existent lead")
	}
}

func TestResumeLead_Blacklisted(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, domain, global_status) VALUES ('a@x.com', 'x.com', 'blacklisted')")

	_, err := ResumeLead(db, "a@x.com")
	if err == nil {
		t.Error("expected error for blacklisted lead")
	}
}

func TestResumeLead_SkipsCompletedCampaigns(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file, status) VALUES ('active-camp', 'seq.yml', 'active')")
	db.Exec("INSERT INTO campaigns (name, sequence_file, status) VALUES ('completed-camp', 'seq.yml', 'completed')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'paused')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (2, 1, 'paused')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'cancelled')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (2, 1, 1, 1, '2025-01-01', 'cancelled')")

	result, err := ResumeLead(db, "john@acme.com")
	if err != nil {
		t.Fatalf("ResumeLead error: %v", err)
	}
	// Should only resume for the active campaign
	if result.ResumedCampaigns != 1 {
		t.Errorf("expected 1 resumed campaign (active only), got %d", result.ResumedCampaigns)
	}
}

func TestRemoveLeadFromCampaign_HappyPath(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04', 'pending')")

	result, err := RemoveLeadFromCampaign(db, "c1", "john@acme.com")
	if err != nil {
		t.Fatalf("RemoveLeadFromCampaign error: %v", err)
	}
	if result.CancelledSends != 2 {
		t.Errorf("expected 2 cancelled sends, got %d", result.CancelledSends)
	}
	if result.Campaign != "c1" {
		t.Errorf("expected campaign 'c1', got %q", result.Campaign)
	}

	// Verify campaign_lead removed
	var count int
	db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = 1 AND lead_id = 1").Scan(&count)
	if count != 0 {
		t.Errorf("expected campaign_lead to be deleted, got %d", count)
	}

	// Verify sends cancelled
	var pending int
	db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE campaign_id = 1 AND lead_id = 1 AND status = 'pending'").Scan(&pending)
	if pending != 0 {
		t.Errorf("expected 0 pending sends, got %d", pending)
	}
}

func TestRemoveLeadFromCampaign_NotInCampaign(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('john@acme.com', 'acme.com')")

	_, err := RemoveLeadFromCampaign(db, "c1", "john@acme.com")
	if err == nil {
		t.Error("expected error for lead not in campaign")
	}
}

func TestRemoveLeadFromCampaign_CampaignNotFound(t *testing.T) {
	db := testDB(t)

	_, err := RemoveLeadFromCampaign(db, "nonexistent", "john@acme.com")
	if err == nil {
		t.Error("expected error for non-existent campaign")
	}
}

func TestRemoveLeadFromCampaign_LeadNotFound(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")

	_, err := RemoveLeadFromCampaign(db, "c1", "nonexistent@x.com")
	if err == nil {
		t.Error("expected error for non-existent lead")
	}
}

func TestBlacklistLead_ByEmail(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('jane@acme.com', 'Jane', 'acme.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04', 'pending')")

	result, err := BlacklistLead(db, "john@acme.com")
	if err != nil {
		t.Fatalf("BlacklistLead error: %v", err)
	}
	if result.IsDomain {
		t.Error("expected IsDomain=false for email blacklist")
	}
	if result.BlacklistedLeads != 1 {
		t.Errorf("expected 1 blacklisted, got %d", result.BlacklistedLeads)
	}
	if result.CancelledSends != 2 {
		t.Errorf("expected 2 cancelled, got %d", result.CancelledSends)
	}

	// jane should NOT be affected
	var janeStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE email = 'jane@acme.com'").Scan(&janeStatus)
	if janeStatus != "active" {
		t.Errorf("expected jane to remain active, got %q", janeStatus)
	}
}

func TestBlacklistLead_ByDomain(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('jane@acme.com', 'Jane', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, domain) VALUES ('bob@other.com', 'Bob', 'other.com')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 2, 1, 1, '2025-01-01', 'pending')")

	result, err := BlacklistLead(db, "acme.com")
	if err != nil {
		t.Fatalf("BlacklistLead error: %v", err)
	}
	if !result.IsDomain {
		t.Error("expected IsDomain=true for domain blacklist")
	}
	if result.BlacklistedLeads != 2 {
		t.Errorf("expected 2 blacklisted, got %d", result.BlacklistedLeads)
	}
	if result.CancelledSends != 2 {
		t.Errorf("expected 2 cancelled, got %d", result.CancelledSends)
	}

	// bob on different domain should NOT be affected
	var bobStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE email = 'bob@other.com'").Scan(&bobStatus)
	if bobStatus != "active" {
		t.Errorf("expected bob to remain active, got %q", bobStatus)
	}
}

func TestBlacklistLead_NotFound(t *testing.T) {
	db := testDB(t)

	_, err := BlacklistLead(db, "nonexistent@x.com")
	if err == nil {
		t.Error("expected error for non-existent lead")
	}
}

func TestBlacklistLead_AlreadyBlacklisted(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, domain, global_status) VALUES ('a@x.com', 'x.com', 'blacklisted')")

	_, err := BlacklistLead(db, "a@x.com")
	if err == nil {
		t.Error("expected error for already blacklisted lead")
	}
}

func TestListLeads_All(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('a@acme.com', 'Alice', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('b@acme.com', 'Bob', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain, global_status) VALUES ('c@other.com', 'Carol', 'Other', 'other.com', 'blacklisted')")

	leads, err := ListLeads(db, "", "", 50)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 3 {
		t.Errorf("expected 3 leads, got %d", len(leads))
	}
}

func TestListLeads_FilterByDomain(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('a@acme.com', 'Alice', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('b@acme.com', 'Bob', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('c@other.com', 'Carol', 'Other', 'other.com')")

	leads, err := ListLeads(db, "acme.com", "", 50)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 2 {
		t.Errorf("expected 2 acme.com leads, got %d", len(leads))
	}
}

func TestListLeads_FilterByStatus(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain, global_status) VALUES ('b@x.com', 'x.com', 'blacklisted')")

	leads, err := ListLeads(db, "", "blacklisted", 50)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 1 {
		t.Errorf("expected 1 blacklisted lead, got %d", len(leads))
	}
	if leads[0].Email != "b@x.com" {
		t.Errorf("expected b@x.com, got %s", leads[0].Email)
	}
}

func TestListLeads_Limit(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('c@x.com', 'x.com')")

	leads, err := ListLeads(db, "", "", 1)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 1 {
		t.Errorf("expected 1 lead with limit=1, got %d", len(leads))
	}
}

func TestListLeads_WithCampaignCount(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c2', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (2, 1, 'active')")

	leads, err := ListLeads(db, "", "", 50)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d", len(leads))
	}
	if leads[0].Campaigns != 2 {
		t.Errorf("expected 2 campaigns, got %d", leads[0].Campaigns)
	}
}
