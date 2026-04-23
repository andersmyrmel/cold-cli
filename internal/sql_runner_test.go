package internal

import (
	"strings"
	"testing"
	"time"
)

func TestRebindSQL_Postgres(t *testing.T) {
	query := `
		SELECT * FROM accounts
		WHERE email = ? AND note = '?' AND name = "?"
		-- comment ?
		AND status = ?
		/* block ? */
		AND daily_limit >= ?`

	got := rebindSQL(DialectPostgres, query)

	if strings.Contains(got, "email = ?") {
		t.Fatalf("expected first placeholder to be rebound: %q", got)
	}
	if !strings.Contains(got, "email = $1") {
		t.Fatalf("expected $1 placeholder: %q", got)
	}
	if !strings.Contains(got, "status = $2") {
		t.Fatalf("expected $2 placeholder: %q", got)
	}
	if !strings.Contains(got, "daily_limit >= $3") {
		t.Fatalf("expected $3 placeholder: %q", got)
	}
	if !strings.Contains(got, "note = '?'") {
		t.Fatalf("expected quoted literal to remain unchanged: %q", got)
	}
	if !strings.Contains(got, `name = "?"`) {
		t.Fatalf("expected quoted identifier to remain unchanged: %q", got)
	}
	if !strings.Contains(got, "-- comment ?") {
		t.Fatalf("expected line comment to remain unchanged: %q", got)
	}
	if !strings.Contains(got, "/* block ? */") {
		t.Fatalf("expected block comment to remain unchanged: %q", got)
	}
}

func TestAccountFlow_SQLite(t *testing.T) {
	db := testDB(t)

	added, err := AddAccount(db, "sqlite-flow@example.com", 25, "/tmp/sqlite-flow")
	if err != nil {
		t.Fatalf("adding account: %v", err)
	}
	if added.ID == 0 {
		t.Fatal("expected inserted account ID")
	}

	newLimit := 40
	if err := UpdateAccount(db, added.Email, UpdateAccountOpts{DailyLimit: &newLimit}); err != nil {
		t.Fatalf("updating account: %v", err)
	}

	accounts, err := ListAccounts(db)
	if err != nil {
		t.Fatalf("listing accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Email != added.Email {
		t.Fatalf("expected %q, got %q", added.Email, accounts[0].Email)
	}
	if accounts[0].DailyLimit != newLimit {
		t.Fatalf("expected updated daily limit %d, got %d", newLimit, accounts[0].DailyLimit)
	}
}

func TestCreateCampaign_PostgresModeBoundary(t *testing.T) {
	db := testDB(t)
	registerDBDialect(db, DialectPostgres)
	t.Cleanup(func() { unregisterDBDialect(db) })

	if _, err := AddAccount(db, "sender@example.com", 50, "/tmp/postgres-boundary"); err != nil {
		t.Fatalf("adding account: %v", err)
	}

	seqYAML := `name: Boundary
steps:
  - step: 1
    subject: "Hello {{first_name}}"
    body: "Hi {{first_name}}, checking in from {{company}}."
`
	leadsCSV := "email,first_name,company\nalice@example.com,Alice,Acme\n"

	result, err := CreateCampaign(db, CreateCampaignOpts{
		Name:           "pg-boundary",
		SequenceInline: seqYAML,
		LeadsInline:    leadsCSV,
		AccountEmails:  []string{"sender@example.com"},
	})
	if err != nil {
		t.Fatalf("creating campaign in postgres mode boundary: %v", err)
	}
	if result.ID == 0 {
		t.Fatal("expected campaign ID")
	}
	if result.Leads != 1 || result.ScheduledSends != 1 {
		t.Fatalf("unexpected campaign result: %+v", result)
	}

	var status string
	if err := queryRowDB(db, "SELECT status FROM campaigns WHERE id = ?", result.ID).Scan(&status); err != nil {
		t.Fatalf("loading stored campaign: %v", err)
	}
	if status != "draft" {
		t.Fatalf("expected draft status, got %q", status)
	}
}

func TestTick_PostgresModeBoundary(t *testing.T) {
	db, campaignID, accountIDs, leadIDs := setupTickTestDB(t)
	registerDBDialect(db, DialectPostgres)
	t.Cleanup(func() { unregisterDBDialect(db) })

	now := time.Now().UTC()
	insertPendingSend(t, db, campaignID, leadIDs[0], accountIDs[0], 1, now.Add(-1*time.Hour))

	mock := &MockGWS{}
	result, err := Tick(TickConfig{
		DB:      db,
		GWS:     mock,
		Now:     now,
		NoSleep: true,
	})
	if err != nil {
		t.Fatalf("tick in postgres mode boundary: %v", err)
	}
	if result.Sent != 1 {
		t.Fatalf("expected 1 sent email, got %d", result.Sent)
	}
	if len(mock.SentEmails) != 1 {
		t.Fatalf("expected 1 outbound send, got %d", len(mock.SentEmails))
	}

	var status string
	if err := queryRowDB(db, "SELECT status FROM scheduled_sends WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadIDs[0]).Scan(&status); err != nil {
		t.Fatalf("loading send status: %v", err)
	}
	if status != "sent" {
		t.Fatalf("expected sent status, got %q", status)
	}
}

func TestCloneCampaign_PostgresModeBoundary(t *testing.T) {
	db := testDB(t)
	registerDBDialect(db, DialectPostgres)
	t.Cleanup(func() { unregisterDBDialect(db) })

	added, err := AddAccount(db, "clone-sender@example.com", 50, "/tmp/postgres-clone")
	if err != nil {
		t.Fatalf("adding account: %v", err)
	}

	seqYAML := `name: Source
steps:
  - step: 1
    subject: "Hello {{first_name}}"
    body: "Hi {{first_name}}, reaching out from {{company}}."
`

	var sourceID int64
	err = queryRowDB(db, `
		INSERT INTO campaigns (
			name, status, sequence_file, sequence_content, stop_on_reply, stop_on_domain_reply,
			send_window_start, send_window_end, send_days, timezone, min_gap_seconds, max_gap_seconds
		) VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		"pg-clone-source", "seq.yml", seqYAML, 1, 1, "09:00", "17:00", "1,2,3,4,5", "UTC", 90, 140,
	).Scan(&sourceID)
	if err != nil {
		t.Fatalf("inserting source campaign: %v", err)
	}

	if _, err := execDB(db, "INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", sourceID, added.ID); err != nil {
		t.Fatalf("linking source account: %v", err)
	}

	leadsCSV := "email,first_name,company\ncloned@example.com,Clara,CloneCo\n"
	result, err := CloneCampaign(db, CloneCampaignOpts{
		SourceName:  "pg-clone-source",
		NewName:     "pg-clone-copy",
		LeadsInline: leadsCSV,
	})
	if err != nil {
		t.Fatalf("cloning campaign in postgres mode boundary: %v", err)
	}
	if result.ID == 0 {
		t.Fatal("expected cloned campaign ID")
	}

	var stopOnReply, stopOnDomainReply int
	err = queryRowDB(db, "SELECT stop_on_reply, stop_on_domain_reply FROM campaigns WHERE id = ?", result.ID).
		Scan(&stopOnReply, &stopOnDomainReply)
	if err != nil {
		t.Fatalf("loading cloned campaign flags: %v", err)
	}
	if stopOnReply != 1 || stopOnDomainReply != 1 {
		t.Fatalf("expected cloned stop flags to be 1/1, got %d/%d", stopOnReply, stopOnDomainReply)
	}
}
