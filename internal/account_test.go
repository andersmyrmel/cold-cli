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
