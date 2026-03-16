package internal

import (
	"database/sql"
	"strings"
	"testing"
)

func TestAccountAdd(t *testing.T) {
	db := testDB(t)

	_, err := db.Exec("INSERT INTO accounts (email, daily_limit) VALUES (?, ?)", "test@example.com", 50)
	if err != nil {
		t.Fatalf("inserting account: %v", err)
	}

	var email string
	var dailyLimit int
	var status string
	err = db.QueryRow("SELECT email, daily_limit, status FROM accounts WHERE email = ?", "test@example.com").
		Scan(&email, &dailyLimit, &status)
	if err != nil {
		t.Fatalf("querying account: %v", err)
	}

	if email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", email)
	}
	if dailyLimit != 50 {
		t.Errorf("expected daily_limit 50, got %d", dailyLimit)
	}
	if status != "active" {
		t.Errorf("expected status active, got %s", status)
	}
}

func TestAccountDuplicate(t *testing.T) {
	db := testDB(t)

	_, err := db.Exec("INSERT INTO accounts (email) VALUES (?)", "dup@example.com")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = db.Exec("INSERT INTO accounts (email) VALUES (?)", "dup@example.com")
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("expected UNIQUE constraint error, got: %v", err)
	}
}

func TestLeadBlacklist_ByEmail(t *testing.T) {
	db := testDB(t)
	setupLeadTestData(t, db)

	// Blacklist by email
	_, err := db.Exec("UPDATE leads SET global_status = 'blacklisted' WHERE email = ?", "john@acme.com")
	if err != nil {
		t.Fatalf("blacklisting: %v", err)
	}

	var status string
	db.QueryRow("SELECT global_status FROM leads WHERE email = ?", "john@acme.com").Scan(&status)
	if status != "blacklisted" {
		t.Errorf("expected blacklisted, got %s", status)
	}

	// Other lead on same domain should NOT be affected
	db.QueryRow("SELECT global_status FROM leads WHERE email = ?", "jane@acme.com").Scan(&status)
	if status != "active" {
		t.Errorf("expected jane to remain active, got %s", status)
	}
}

func TestLeadBlacklist_ByDomain(t *testing.T) {
	db := testDB(t)
	setupLeadTestData(t, db)

	// Blacklist by domain
	res, err := db.Exec("UPDATE leads SET global_status = 'blacklisted' WHERE domain = ?", "acme.com")
	if err != nil {
		t.Fatalf("blacklisting domain: %v", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 2 {
		t.Errorf("expected 2 leads blacklisted, got %d", affected)
	}

	// Lead on different domain should NOT be affected
	var status string
	db.QueryRow("SELECT global_status FROM leads WHERE email = ?", "bob@other.com").Scan(&status)
	if status != "active" {
		t.Errorf("expected bob to remain active, got %s", status)
	}
}

func TestLeadBlacklist_CancelsPendingSends(t *testing.T) {
	db := testDB(t)
	setupLeadTestData(t, db)
	setupScheduledSendsTestData(t, db)

	// Blacklist john@acme.com — should cancel his pending sends
	db.Exec("UPDATE leads SET global_status = 'blacklisted' WHERE email = ?", "john@acme.com")
	res, _ := db.Exec(`
		UPDATE scheduled_sends SET status = 'cancelled'
		WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
		AND status = 'pending'`,
		"john@acme.com",
	)
	cancelled, _ := res.RowsAffected()
	if cancelled != 2 {
		t.Errorf("expected 2 sends cancelled, got %d", cancelled)
	}

	// Verify sends are cancelled
	var pendingCount int
	db.QueryRow(`
		SELECT COUNT(*) FROM scheduled_sends
		WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
		AND status = 'pending'`,
		"john@acme.com",
	).Scan(&pendingCount)
	if pendingCount != 0 {
		t.Errorf("expected 0 pending sends after blacklist, got %d", pendingCount)
	}
}

func TestLeadPause(t *testing.T) {
	db := testDB(t)
	setupLeadTestData(t, db)
	setupScheduledSendsTestData(t, db)

	// Add campaign_lead entry
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'active')")

	// Pause lead
	db.Exec("UPDATE campaign_leads SET status = 'paused' WHERE lead_id = 1 AND status IN ('active', 'pending')")
	res, _ := db.Exec("UPDATE scheduled_sends SET status = 'cancelled' WHERE lead_id = 1 AND status = 'pending'")

	cancelled, _ := res.RowsAffected()
	if cancelled != 2 {
		t.Errorf("expected 2 sends cancelled, got %d", cancelled)
	}

	var clStatus string
	db.QueryRow("SELECT status FROM campaign_leads WHERE lead_id = 1").Scan(&clStatus)
	if clStatus != "paused" {
		t.Errorf("expected campaign_lead status 'paused', got %s", clStatus)
	}
}

func setupLeadTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"INSERT INTO leads (email, first_name, domain) VALUES ('john@acme.com', 'John', 'acme.com')",
		"INSERT INTO leads (email, first_name, domain) VALUES ('jane@acme.com', 'Jane', 'acme.com')",
		"INSERT INTO leads (email, first_name, domain) VALUES ('bob@other.com', 'Bob', 'other.com')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("setup lead: %v", err)
		}
	}
}

func setupScheduledSendsTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	// Need a campaign and account first
	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('test-campaign', 'seq.yml')")

	for _, q := range []string{
		"INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01 09:00:00', 'pending')",
		"INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04 09:00:00', 'pending')",
		"INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 2, 1, 1, '2025-01-01 09:02:00', 'pending')",
		"INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 3, 1, 1, '2025-01-01 09:04:00', 'pending')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("setup scheduled_send: %v", err)
		}
	}
}

func TestPauseAccount(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('lead@x.com', 'x.com')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 2, '2025-01-04', 'pending')")

	result, err := PauseAccount(db, "sender@x.com")
	if err != nil {
		t.Fatalf("PauseAccount error: %v", err)
	}
	if result.CancelledSends != 2 {
		t.Errorf("expected 2 cancelled sends, got %d", result.CancelledSends)
	}

	var status string
	db.QueryRow("SELECT status FROM accounts WHERE email = 'sender@x.com'").Scan(&status)
	if status != "paused" {
		t.Errorf("expected account status 'paused', got %q", status)
	}

	// Pause again should error
	_, err = PauseAccount(db, "sender@x.com")
	if err == nil {
		t.Error("expected error pausing already-paused account")
	}
}

func TestResumeAccount(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, status) VALUES ('sender@x.com', 'paused')")

	err := ResumeAccount(db, "sender@x.com")
	if err != nil {
		t.Fatalf("ResumeAccount error: %v", err)
	}

	var status string
	db.QueryRow("SELECT status FROM accounts WHERE email = 'sender@x.com'").Scan(&status)
	if status != "active" {
		t.Errorf("expected 'active', got %q", status)
	}

	// Resume active should error
	err = ResumeAccount(db, "sender@x.com")
	if err == nil {
		t.Error("expected error resuming active account")
	}
}

func TestRemoveAccount(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 50)")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('lead@x.com', 'x.com')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-01', 'pending')")

	result, err := RemoveAccount(db, "sender@x.com")
	if err != nil {
		t.Fatalf("RemoveAccount error: %v", err)
	}
	if result.CancelledSends != 1 {
		t.Errorf("expected 1 cancelled, got %d", result.CancelledSends)
	}

	// Account should be marked 'removed' (kept for historical reference)
	var status string
	db.QueryRow("SELECT status FROM accounts WHERE email = 'sender@x.com'").Scan(&status)
	if status != "removed" {
		t.Errorf("expected status 'removed', got %q", status)
	}

	// campaign_accounts link should be gone
	var count int
	db.QueryRow("SELECT COUNT(*) FROM campaign_accounts WHERE account_id = 1").Scan(&count)
	if count != 0 {
		t.Error("expected campaign_accounts link to be deleted")
	}
}

func TestReAddRemovedAccount(t *testing.T) {
	db := testDB(t)

	// Add and remove
	_, err := AddAccount(db, "sender@x.com", 50, "/tmp/gws")
	if err != nil {
		t.Fatalf("AddAccount error: %v", err)
	}
	_, err = RemoveAccount(db, "sender@x.com")
	if err != nil {
		t.Fatalf("RemoveAccount error: %v", err)
	}

	// Re-add should succeed and reactivate
	result, err := AddAccount(db, "sender@x.com", 30, "/tmp/gws2")
	if err != nil {
		t.Fatalf("Re-add error: %v", err)
	}
	if result.Status != "active" {
		t.Errorf("expected active, got %s", result.Status)
	}
	if result.DailyLimit != 30 {
		t.Errorf("expected daily_limit 30, got %d", result.DailyLimit)
	}

	// Verify in DB
	var status string
	var limit int
	db.QueryRow("SELECT status, daily_limit FROM accounts WHERE email = 'sender@x.com'").Scan(&status, &limit)
	if status != "active" {
		t.Errorf("expected active in DB, got %s", status)
	}
	if limit != 30 {
		t.Errorf("expected 30 in DB, got %d", limit)
	}
}

func TestUpdateAccount(t *testing.T) {
	db := testDB(t)
	AddAccount(db, "sender@x.com", 50, "/tmp/gws")

	// Update daily limit
	newLimit := 25
	err := UpdateAccount(db, "sender@x.com", UpdateAccountOpts{DailyLimit: &newLimit})
	if err != nil {
		t.Fatalf("UpdateAccount error: %v", err)
	}

	var limit int
	db.QueryRow("SELECT daily_limit FROM accounts WHERE email = 'sender@x.com'").Scan(&limit)
	if limit != 25 {
		t.Errorf("expected 25, got %d", limit)
	}

	// Invalid limit
	badLimit := 0
	err = UpdateAccount(db, "sender@x.com", UpdateAccountOpts{DailyLimit: &badLimit})
	if err == nil {
		t.Error("expected error for zero daily limit")
	}

	// Non-existent account
	err = UpdateAccount(db, "nope@x.com", UpdateAccountOpts{DailyLimit: &newLimit})
	if err == nil {
		t.Error("expected error for non-existent account")
	}
}

func TestDailyLimitWarnings(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO accounts (email, daily_limit) VALUES ('sender@x.com', 2)")
	db.Exec("INSERT INTO campaigns (name, sequence_file, status) VALUES ('c1', 'seq.yml', 'active')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('c@x.com', 'x.com')")

	// 3 sends on same day, limit is 2
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 1, 1, 1, '2025-01-15 09:00:00', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 2, 1, 1, '2025-01-15 09:05:00', 'pending')")
	db.Exec("INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status) VALUES (1, 3, 1, 1, '2025-01-15 09:10:00', 'pending')")

	warnings, err := GetDailyLimitWarnings(db)
	if err != nil {
		t.Fatalf("GetDailyLimitWarnings error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if warnings[0].Scheduled != 3 {
		t.Errorf("expected 3 scheduled, got %d", warnings[0].Scheduled)
	}
	if warnings[0].Limit != 2 {
		t.Errorf("expected limit 2, got %d", warnings[0].Limit)
	}
	if warnings[0].Overflow != 1 {
		t.Errorf("expected overflow 1, got %d", warnings[0].Overflow)
	}
}

func TestListLeads(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('a@acme.com', 'Alice', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain) VALUES ('b@acme.com', 'Bob', 'Acme', 'acme.com')")
	db.Exec("INSERT INTO leads (email, first_name, company, domain, global_status) VALUES ('c@other.com', 'Carol', 'Other', 'other.com', 'blacklisted')")

	// All leads
	leads, err := ListLeads(db, "", "", 50)
	if err != nil {
		t.Fatalf("ListLeads error: %v", err)
	}
	if len(leads) != 3 {
		t.Errorf("expected 3 leads, got %d", len(leads))
	}

	// Filter by domain
	leads, _ = ListLeads(db, "acme.com", "", 50)
	if len(leads) != 2 {
		t.Errorf("expected 2 acme.com leads, got %d", len(leads))
	}

	// Filter by status
	leads, _ = ListLeads(db, "", "blacklisted", 50)
	if len(leads) != 1 {
		t.Errorf("expected 1 blacklisted lead, got %d", len(leads))
	}
	if leads[0].Email != "c@other.com" {
		t.Errorf("expected c@other.com, got %s", leads[0].Email)
	}

	// Limit
	leads, _ = ListLeads(db, "", "", 1)
	if len(leads) != 1 {
		t.Errorf("expected 1 lead with limit=1, got %d", len(leads))
	}
}
