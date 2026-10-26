package internal

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeTestContactCheck(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contact-check")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func configureTestContactCheck(t *testing.T, workspace, command string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", dir)
	config := "pre_send_checks:\n  " + workspace + ":\n    command: [" + `"` + command + `"` + "]\n    timeout_seconds: 20\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSendCheckContactsIncludesCcAndPhoneAliases(t *testing.T) {
	got, err := sendCheckContacts(EmailParams{
		ToEmail:  "Doctor <DOCTOR@example.com>",
		CcEmails: []string{"office@example.com", "doctor@example.com"},
	}, map[string]string{
		"phone":         "+1 202 555 0100",
		"phone_numbers": `["+12025550100","+12025550101"]`,
		"mobile":        "+12025550102; +12025550103",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"doctor@example.com", "office@example.com", "+1 202 555 0100", "+12025550100", "+12025550101", "+12025550102", "+12025550103"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contacts = %#v, want %#v", got, want)
	}
	if _, err := sendCheckContacts(EmailParams{ToEmail: "bad recipient"}, nil); err == nil {
		t.Fatal("expected invalid recipient to block")
	}
	if _, err := sendCheckContacts(EmailParams{ToEmail: "a@example.com"}, map[string]string{"phones": "[invalid"}); err == nil {
		t.Fatal("expected malformed phone list to block")
	}
}

func TestRunPreSendCheckFailsClosedWithoutLeakingOutput(t *testing.T) {
	for _, tc := range []struct {
		name, script        string
		wantErr, suppressed bool
	}{
		{"clear", "cat >/dev/null; exit 0", false, false},
		{"suppressed", "cat >/dev/null; echo private-opt-out-note >&2; exit 2", true, true},
		{"unavailable", "echo database-password >&2; exit 1", true, false},
		{"timeout", "exec sleep 3", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timeoutSeconds := 20
			if tc.name == "timeout" {
				timeoutSeconds = 1
			}
			check := PreSendCheckConfig{Command: []string{writeTestContactCheck(t, tc.script)}, TimeoutSeconds: timeoutSeconds}
			err := runPreSendCheck(check, []string{"a@example.com"})
			if (err != nil) != tc.wantErr || errors.Is(err, ErrContactSuppressed) != tc.suppressed {
				t.Fatalf("unexpected check result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "private-opt-out-note") || strings.Contains(err.Error(), "database-password")) {
				t.Fatalf("checker output leaked: %v", err)
			}
		})
	}
	for _, check := range []PreSendCheckConfig{{}, {Command: []string{"/does/not/exist"}}, {Command: []string{"true"}, TimeoutSeconds: -1}} {
		if err := runPreSendCheck(check, []string{"a@example.com"}); err == nil {
			t.Fatal("expected invalid or missing command to block")
		}
	}
}

func TestPreSendCheckRunsFreshForEveryDeliveryAndScopesWorkspace(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "blocked")
	t.Setenv("TEST_CONTACT_BLOCK_FLAG", flag)
	command := writeTestContactCheck(t, `cat >/dev/null
if [ -f "$TEST_CONTACT_BLOCK_FLAG" ]; then exit 2; fi
exit 0`)
	configureTestContactCheck(t, "workspace-a", command)
	account := Account{Email: "sender@example.com", Provider: AccountProviderGWS, WorkspaceID: "workspace-a"}
	params := EmailParams{FromEmail: account.Email, ToEmail: "a@example.com", Subject: "Hi", Body: "Hello"}
	gws := &MockGWS{}
	if _, _, err := sendRenderedEmail(TickConfig{GWS: gws}, account, params, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flag, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	params.InReplyTo = "<previous@example.com>"
	if _, _, err := sendRenderedEmail(TickConfig{GWS: gws}, account, params, nil); !errors.Is(err, ErrContactSuppressed) {
		t.Fatalf("follow-up should see fresh suppression: %v", err)
	}
	if len(gws.SentEmails) != 1 {
		t.Fatal("suppressed follow-up reached sender")
	}
	account.WorkspaceID = "another-project"
	if _, _, err := sendRenderedEmail(TickConfig{GWS: gws}, account, params, nil); err != nil {
		t.Fatalf("unconfigured workspace affected: %v", err)
	}
}

func TestTickPreSendCheckSkipsBothStepsAndFailsClosedOnErrors(t *testing.T) {
	for _, tc := range []struct{ name, script, status string }{
		{"suppressed", "cat >/dev/null; exit 2", "skipped"},
		{"checker-error", "cat >/dev/null; exit 1", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, campaignID, _, _ := setupTickTestDB(t)
			configureTestContactCheck(t, "workspace-a", writeTestContactCheck(t, tc.script))
			if _, err := db.Exec("UPDATE accounts SET workspace_id = 'workspace-a', provider = ?", AccountProviderSMTPIMAP); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE campaigns SET workspace_id = 'workspace-a'"); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
			insertPendingSend(t, db, campaignID, 1, 1, 1, now.Add(-time.Minute))
			insertPendingFollowUpSend(t, db, campaignID, 1, 1, 0, now.AddDate(0, 0, 7), "thread", "<first@example.com>")
			smtp := &MockSMTPEmailSender{}
			result, err := Tick(TickConfig{DB: db, SMTPSender: smtp, IMAP: &MockIMAPMessageLister{}, Now: now, NoSleep: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(smtp.SentEmails) != 0 || result.Sent != 0 {
				t.Fatal("blocked email reached SMTP")
			}
			var status string
			if err := db.QueryRow("SELECT status FROM scheduled_sends WHERE step_number = 1").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != tc.status {
				t.Fatalf("step 1 status = %s, want %s", status, tc.status)
			}
			if tc.name == "suppressed" {
				if err := db.QueryRow("SELECT status FROM scheduled_sends WHERE step_number = 2").Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "skipped" || result.Skipped != 1 {
					t.Fatal("remaining follow-up was not stopped")
				}
			}
		})
	}
}

func TestManualReplyPreSendCheckIncludesChangedRecipientCcAndPhone(t *testing.T) {
	db, campaignID, _, _ := setupTickTestDB(t)
	command := writeTestContactCheck(t, `contacts=$(cat)
case "$contacts" in
  *reply@example.com*office@example.com*+12025550100*) exit 2 ;;
  *) exit 1 ;;
esac`)
	configureTestContactCheck(t, "workspace-a", command)
	if _, err := db.Exec("UPDATE accounts SET workspace_id = 'workspace-a', provider = ?", AccountProviderSMTPIMAP); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE campaigns SET workspace_id = 'workspace-a'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE leads SET custom_fields = '{"phone":"+12025550100"}' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := insertEmailMessage(db, EmailMessage{
		CampaignID: campaignID, LeadID: 1, AccountID: 1,
		Direction: EmailMessageDirectionInbound, Type: EmailMessageTypeReply,
		MessageID: "inbound", ThreadID: "thread", FromEmail: "reply@example.com",
		ToEmails: "sender@x.com", CcEmails: "office@example.com", Subject: "Question", TextBody: "Hello",
		RawHeaders: `{"Message-ID":"<inbound@example.com>","Cc":"office@example.com"}`,
		OccurredAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	smtp := &MockSMTPEmailSender{}
	_, err := SendInboxReply(SendInboxReplyConfig{
		DB: db, WorkspaceID: "workspace-a", CampaignID: campaignID, LeadID: 1,
		Body: "A reply", ReplyAll: true, SMTPSender: smtp,
	})
	if !errors.Is(err, ErrContactSuppressed) {
		t.Fatalf("expected suppression of actual To/Cc/phone, got %v", err)
	}
	if len(smtp.SentEmails) != 0 {
		t.Fatal("blocked manual reply reached SMTP")
	}
}

func TestSkipSuppressedCampaignLeadPostgresBoundary(t *testing.T) {
	db, campaignID, _, _ := setupTickTestDB(t)
	registerDBDialect(db, DialectPostgres)
	t.Cleanup(func() { unregisterDBDialect(db) })
	now := time.Now()
	insertPendingSend(t, db, campaignID, 1, 1, 1, now)
	insertPendingSend(t, db, campaignID, 1, 1, 2, now.AddDate(0, 0, 7))
	insertPendingSend(t, db, campaignID, 2, 1, 1, now)
	if err := skipSuppressedCampaignLead(db, campaignID, 1); err != nil {
		t.Fatalf("suppression transaction with Postgres placeholders: %v", err)
	}
	var skipped, unaffected int
	if err := db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE lead_id = 1 AND status = 'skipped'").Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM scheduled_sends WHERE lead_id = 2 AND status = 'pending'").Scan(&unaffected); err != nil {
		t.Fatal(err)
	}
	if skipped != 2 || unaffected != 1 {
		t.Fatalf("suppression crossed lead boundary: skipped=%d unaffected=%d", skipped, unaffected)
	}
}

func TestPreSendCheckRejectsInvalidRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COLD_CLI_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("pre_send_checks: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	gws := &MockGWS{}
	_, _, err := sendRenderedEmail(TickConfig{GWS: gws},
		Account{WorkspaceID: "workspace-a", Email: "sender@example.com", Provider: AccountProviderGWS},
		EmailParams{FromEmail: "sender@example.com", ToEmail: "a@example.com", Subject: "Hi", Body: "Hello"}, nil)
	if err == nil || len(gws.SentEmails) != 0 {
		t.Fatalf("invalid runtime configuration did not block delivery: %v", err)
	}
}
