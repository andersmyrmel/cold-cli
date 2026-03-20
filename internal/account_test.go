package internal

import (
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
