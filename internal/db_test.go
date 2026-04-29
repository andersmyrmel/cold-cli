package internal

import (
	"testing"
)

func TestOpenDB(t *testing.T) {
	db := testDB(t)

	// Verify all tables exist
	tables := []string{
		"accounts", "campaigns", "campaign_accounts",
		"leads", "campaign_leads", "scheduled_sends", "events", "kv",
	}
	for _, table := range tables {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestOpenDB_Indexes(t *testing.T) {
	db := testDB(t)

	indexes := []string{
		"idx_sends_pending",
		"idx_events_account_day",
		"idx_events_message_id",
		"idx_leads_email",
		"idx_leads_domain",
	}
	for _, idx := range indexes {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

func TestOpenDB_ForeignKeys(t *testing.T) {
	db := testDB(t)

	var enabled int
	err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled)
	if err != nil {
		t.Fatalf("checking foreign_keys pragma: %v", err)
	}
	if enabled != 1 {
		t.Error("foreign_keys not enabled")
	}
}

func TestOpenDB_Idempotent(t *testing.T) {
	db := testDB(t)

	// Insert a row
	_, err := db.Exec("INSERT INTO accounts (email) VALUES (?)", "test@example.com")
	if err != nil {
		t.Fatalf("inserting account: %v", err)
	}

	// Re-running schema should not error or lose data (CREATE IF NOT EXISTS)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("re-running schema: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 account after re-migration, got %d", count)
	}
}

func TestOpenDB_TableColumns(t *testing.T) {
	db := testDB(t)

	// Verify scheduled_sends has all expected columns
	cols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(scheduled_sends)")
	if err != nil {
		t.Fatalf("getting table info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt *string
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scanning column: %v", err)
		}
		cols[name] = true
	}

	expected := []string{
		"id", "campaign_id", "lead_id", "account_id",
		"step_number", "variant_index", "send_at", "status",
		"thread_id", "parent_message_id", "message_id", "sent_at", "error_message",
	}
	for _, col := range expected {
		if !cols[col] {
			t.Errorf("scheduled_sends missing column %q", col)
		}
	}
}

func TestOpenDB_AccountProviderColumns(t *testing.T) {
	db := testDB(t)

	cols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(accounts)")
	if err != nil {
		t.Fatalf("getting account table info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt *string
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scanning account column: %v", err)
		}
		cols[name] = true
	}

	expected := []string{
		"provider", "gws_config_dir",
		"smtp_host", "smtp_port", "smtp_username", "smtp_password_ref", "smtp_tls_mode",
		"imap_host", "imap_port", "imap_username", "imap_password_ref", "imap_tls_mode",
	}
	for _, col := range expected {
		if !cols[col] {
			t.Errorf("accounts missing column %q", col)
		}
	}
}

func TestOpenDB_AccountUniqueEmail(t *testing.T) {
	db := testDB(t)

	_, err := db.Exec("INSERT INTO accounts (email) VALUES (?)", "a@x.com")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = db.Exec("INSERT INTO accounts (email) VALUES (?)", "a@x.com")
	if err == nil {
		t.Error("expected unique constraint violation on duplicate email")
	}
}

func TestOpenDB_CampaignUniqueName(t *testing.T) {
	db := testDB(t)

	_, err := db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES (?, ?)", "test", "seq.yml")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES (?, ?)", "test", "seq2.yml")
	if err == nil {
		t.Error("expected unique constraint violation on duplicate campaign name")
	}
}

func TestOpenDB_LeadUniqueEmail(t *testing.T) {
	db := testDB(t)

	_, err := db.Exec("INSERT INTO leads (email) VALUES (?)", "lead@x.com")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	_, err = db.Exec("INSERT INTO leads (email) VALUES (?)", "lead@x.com")
	if err == nil {
		t.Error("expected unique constraint violation on duplicate lead email")
	}
}

func TestOpenDB_ScheduledSendDefaults(t *testing.T) {
	db := testDB(t)

	// Set up required foreign key rows
	db.Exec("INSERT INTO accounts (email) VALUES ('a@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c', 'seq.yml')")
	db.Exec("INSERT INTO leads (email) VALUES ('l@x.com')")

	_, err := db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at)
		VALUES (1, 1, 1, 1, '2025-01-01 09:00:00')`)
	if err != nil {
		t.Fatalf("inserting scheduled_send: %v", err)
	}

	var status string
	var variantIndex int
	var threadID, parentMsgID, msgID string
	err = db.QueryRow("SELECT status, variant_index, thread_id, parent_message_id, message_id FROM scheduled_sends WHERE id=1").
		Scan(&status, &variantIndex, &threadID, &parentMsgID, &msgID)
	if err != nil {
		t.Fatalf("querying defaults: %v", err)
	}

	if status != "pending" {
		t.Errorf("expected default status 'pending', got %q", status)
	}
	if variantIndex != 0 {
		t.Errorf("expected default variant_index 0, got %d", variantIndex)
	}
	if threadID != "" {
		t.Errorf("expected default thread_id '', got %q", threadID)
	}
	if parentMsgID != "" {
		t.Errorf("expected default parent_message_id '', got %q", parentMsgID)
	}
	if msgID != "" {
		t.Errorf("expected default message_id '', got %q", msgID)
	}
}

func TestOpenDB_IgnoresPostgresEnv(t *testing.T) {
	t.Setenv("COLD_CLI_DATABASE_URL", "postgres://user:secret@localhost:5432/cold_cli")

	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("opening sqlite db with postgres env set: %v", err)
	}
	defer db.Close()

	var enabled int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatalf("checking sqlite pragma: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("expected sqlite database, foreign_keys=%d", enabled)
	}
}
