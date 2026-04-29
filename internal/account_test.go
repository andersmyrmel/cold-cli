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
	var provider string
	err = db.QueryRow("SELECT email, daily_limit, status, provider FROM accounts WHERE email = ?", "test@example.com").
		Scan(&email, &dailyLimit, &status, &provider)
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
	if provider != AccountProviderGWS {
		t.Errorf("expected provider %s, got %s", AccountProviderGWS, provider)
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
	if result.Provider != AccountProviderGWS {
		t.Errorf("expected provider %s, got %s", AccountProviderGWS, result.Provider)
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

func TestListAccountsIncludesProvider(t *testing.T) {
	db := testDB(t)

	if _, err := db.Exec(
		`INSERT INTO accounts (
			email,
			daily_limit,
			provider,
			smtp_host,
			smtp_port,
			smtp_username,
			smtp_password_ref,
			smtp_tls_mode,
			imap_host,
			imap_port,
			imap_username,
			imap_password_ref,
			imap_tls_mode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"smtp@example.com",
		25,
		AccountProviderSMTPIMAP,
		"smtp.example.com",
		465,
		"smtp@example.com",
		"secret://smtp",
		"ssl",
		"imap.example.com",
		993,
		"smtp@example.com",
		"secret://imap",
		"ssl",
	); err != nil {
		t.Fatalf("inserting smtp account: %v", err)
	}

	accounts, err := ListAccounts(db)
	if err != nil {
		t.Fatalf("ListAccounts error: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Provider != AccountProviderSMTPIMAP {
		t.Errorf("expected provider %s, got %s", AccountProviderSMTPIMAP, accounts[0].Provider)
	}
}

func TestAddSMTPIMAPAccount(t *testing.T) {
	db := testDB(t)

	result, err := AddSMTPIMAPAccount(db, AddSMTPIMAPAccountOpts{
		Email:           "sender@example.com",
		DailyLimit:      25,
		SMTPHost:        "smtp.example.com",
		SMTPPort:        587,
		SMTPUsername:    "smtp-user",
		SMTPPasswordRef: "env:SMTP_PASSWORD",
		SMTPTLSMode:     "starttls",
		IMAPHost:        "imap.example.com",
		IMAPPort:        993,
		IMAPUsername:    "imap-user",
		IMAPPasswordRef: "env:IMAP_PASSWORD",
		IMAPTLSMode:     "ssl",
	})
	if err != nil {
		t.Fatalf("AddSMTPIMAPAccount error: %v", err)
	}
	if result.Provider != AccountProviderSMTPIMAP {
		t.Errorf("expected provider %s, got %s", AccountProviderSMTPIMAP, result.Provider)
	}
	if result.SMTPHost != "smtp.example.com" || result.SMTPPort != 587 {
		t.Errorf("unexpected smtp result: %#v", result)
	}

	var provider, gwsConfigDir, smtpHost, smtpUsername, smtpPasswordRef, smtpTLS string
	var smtpPort int
	err = db.QueryRow(
		`SELECT provider, gws_config_dir, smtp_host, smtp_port, smtp_username, smtp_password_ref, smtp_tls_mode
		 FROM accounts WHERE email = ?`,
		"sender@example.com",
	).Scan(&provider, &gwsConfigDir, &smtpHost, &smtpPort, &smtpUsername, &smtpPasswordRef, &smtpTLS)
	if err != nil {
		t.Fatalf("querying smtp account: %v", err)
	}
	if provider != AccountProviderSMTPIMAP {
		t.Errorf("expected provider %s, got %s", AccountProviderSMTPIMAP, provider)
	}
	if gwsConfigDir != "" {
		t.Errorf("expected empty gws_config_dir, got %s", gwsConfigDir)
	}
	if smtpHost != "smtp.example.com" || smtpPort != 587 || smtpUsername != "smtp-user" || smtpPasswordRef != "env:SMTP_PASSWORD" || smtpTLS != "starttls" {
		t.Errorf("unexpected stored smtp config: host=%s port=%d user=%s ref=%s tls=%s", smtpHost, smtpPort, smtpUsername, smtpPasswordRef, smtpTLS)
	}
}

func TestAddSMTPIMAPAccountDefaults(t *testing.T) {
	db := testDB(t)

	result, err := AddSMTPIMAPAccount(db, AddSMTPIMAPAccountOpts{
		Email:           "sender@example.com",
		DailyLimit:      50,
		SMTPHost:        "smtp.example.com",
		SMTPPasswordRef: "env:MAIL_PASSWORD",
		IMAPHost:        "imap.example.com",
	})
	if err != nil {
		t.Fatalf("AddSMTPIMAPAccount error: %v", err)
	}
	if result.SMTPPort != 465 {
		t.Errorf("expected default SMTP SSL port 465, got %d", result.SMTPPort)
	}
	if result.IMAPPort != 993 {
		t.Errorf("expected default IMAP SSL port 993, got %d", result.IMAPPort)
	}
	if result.SMTPUsername != "sender@example.com" || result.IMAPUsername != "sender@example.com" {
		t.Errorf("expected usernames to default to email, got smtp=%s imap=%s", result.SMTPUsername, result.IMAPUsername)
	}

	var imapPasswordRef string
	if err := db.QueryRow("SELECT imap_password_ref FROM accounts WHERE email = ?", "sender@example.com").Scan(&imapPasswordRef); err != nil {
		t.Fatalf("querying imap password ref: %v", err)
	}
	if imapPasswordRef != "env:MAIL_PASSWORD" {
		t.Errorf("expected IMAP password ref to default to SMTP ref, got %s", imapPasswordRef)
	}
}

func TestAddSMTPIMAPAccountValidation(t *testing.T) {
	db := testDB(t)

	cases := []struct {
		name string
		opts AddSMTPIMAPAccountOpts
	}{
		{
			name: "missing smtp host",
			opts: AddSMTPIMAPAccountOpts{
				Email:           "sender@example.com",
				DailyLimit:      50,
				SMTPPasswordRef: "env:MAIL_PASSWORD",
				IMAPHost:        "imap.example.com",
			},
		},
		{
			name: "missing smtp password ref",
			opts: AddSMTPIMAPAccountOpts{
				Email:      "sender@example.com",
				DailyLimit: 50,
				SMTPHost:   "smtp.example.com",
				IMAPHost:   "imap.example.com",
			},
		},
		{
			name: "bad tls",
			opts: AddSMTPIMAPAccountOpts{
				Email:           "sender@example.com",
				DailyLimit:      50,
				SMTPHost:        "smtp.example.com",
				SMTPPasswordRef: "env:MAIL_PASSWORD",
				SMTPTLSMode:     "required",
				IMAPHost:        "imap.example.com",
			},
		},
		{
			name: "bad port",
			opts: AddSMTPIMAPAccountOpts{
				Email:           "sender@example.com",
				DailyLimit:      50,
				SMTPHost:        "smtp.example.com",
				SMTPPort:        70000,
				SMTPPasswordRef: "env:MAIL_PASSWORD",
				IMAPHost:        "imap.example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := AddSMTPIMAPAccount(db, tc.opts); err == nil {
				t.Fatal("expected validation error")
			}
		})
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
