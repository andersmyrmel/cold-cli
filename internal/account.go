package internal

import (
	"database/sql"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/likexian/whois"
	whoisparser "github.com/likexian/whois-parser"
)

// AddAccountResult is returned by AddAccount.
type AddAccountResult struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	DailyLimit   int    `json:"daily_limit"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	GWSConfigDir string `json:"gws_config_dir"`
}

// AddSMTPIMAPAccountOpts holds settings for a generic SMTP/IMAP account.
type AddSMTPIMAPAccountOpts struct {
	Email           string
	DailyLimit      int
	SMTPHost        string
	SMTPPort        int
	SMTPUsername    string
	SMTPPasswordRef string
	SMTPTLSMode     string
	IMAPHost        string
	IMAPPort        int
	IMAPUsername    string
	IMAPPasswordRef string
	IMAPTLSMode     string
}

// AddSMTPIMAPAccountResult is returned by AddSMTPIMAPAccount.
type AddSMTPIMAPAccountResult struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	DailyLimit   int    `json:"daily_limit"`
	Status       string `json:"status"`
	Provider     string `json:"provider"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	SMTPTLSMode  string `json:"smtp_tls_mode"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPUsername string `json:"imap_username"`
	IMAPTLSMode  string `json:"imap_tls_mode"`
}

// UpdateSMTPIMAPAccountOpts holds fields to update on a generic SMTP/IMAP account.
type UpdateSMTPIMAPAccountOpts struct {
	DailyLimit      *int
	SMTPHost        *string
	SMTPPort        *int
	SMTPUsername    *string
	SMTPPasswordRef *string
	SMTPTLSMode     *string
	IMAPHost        *string
	IMAPPort        *int
	IMAPUsername    *string
	IMAPPasswordRef *string
	IMAPTLSMode     *string
}

// AddAccount inserts a new sending account into the database.
// If the account was previously removed, it is reactivated with the new settings.
func AddAccount(db *sql.DB, email string, dailyLimit int, configDir string) (*AddAccountResult, error) {
	// Check for existing removed account
	var existingID int64
	var existingStatus string
	err := queryRowDB(db, "SELECT id, status FROM accounts WHERE email = ?", email).Scan(&existingID, &existingStatus)
	if err == nil {
		if existingStatus == "removed" {
			// Reactivate removed account
			_, err := execDB(
				db,
				`UPDATE accounts
				 SET status = 'active',
				     daily_limit = ?,
				     provider = ?,
				     gws_config_dir = ?,
				     smtp_host = '',
				     smtp_port = 0,
				     smtp_username = '',
				     smtp_password_ref = '',
				     smtp_tls_mode = '',
				     imap_host = '',
				     imap_port = 0,
				     imap_username = '',
				     imap_password_ref = '',
				     imap_tls_mode = ''
				 WHERE id = ?`,
				dailyLimit, AccountProviderGWS, configDir, existingID)
			if err != nil {
				return nil, fmt.Errorf("reactivating account: %w", err)
			}
			return &AddAccountResult{
				ID:           existingID,
				Email:        email,
				DailyLimit:   dailyLimit,
				Status:       "active",
				Provider:     AccountProviderGWS,
				GWSConfigDir: configDir,
			}, nil
		}
		return nil, fmt.Errorf("account %s already exists (status: %s)", email, existingStatus)
	}

	var id int64
	err = queryRowDB(
		db,
		"INSERT INTO accounts (email, daily_limit, provider, gws_config_dir) VALUES (?, ?, ?, ?) RETURNING id",
		email, dailyLimit, AccountProviderGWS, configDir,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("adding account: %w", err)
	}
	return &AddAccountResult{
		ID:           id,
		Email:        email,
		DailyLimit:   dailyLimit,
		Status:       "active",
		Provider:     AccountProviderGWS,
		GWSConfigDir: configDir,
	}, nil
}

// AddSMTPIMAPAccount inserts a generic SMTP/IMAP sending account.
// Password fields are stored as references, not raw secret values.
func AddSMTPIMAPAccount(db *sql.DB, opts AddSMTPIMAPAccountOpts) (*AddSMTPIMAPAccountResult, error) {
	normalized, err := normalizeSMTPIMAPAccountOpts(opts)
	if err != nil {
		return nil, err
	}

	var existingID int64
	var existingStatus string
	err = queryRowDB(db, "SELECT id, status FROM accounts WHERE email = ?", normalized.Email).Scan(&existingID, &existingStatus)
	if err == nil {
		if existingStatus == "removed" {
			_, err := execDB(
				db,
				`UPDATE accounts
				 SET status = 'active',
				     daily_limit = ?,
				     provider = ?,
				     gws_config_dir = '',
				     smtp_host = ?,
				     smtp_port = ?,
				     smtp_username = ?,
				     smtp_password_ref = ?,
				     smtp_tls_mode = ?,
				     imap_host = ?,
				     imap_port = ?,
				     imap_username = ?,
				     imap_password_ref = ?,
				     imap_tls_mode = ?
				 WHERE id = ?`,
				normalized.DailyLimit,
				AccountProviderSMTPIMAP,
				normalized.SMTPHost,
				normalized.SMTPPort,
				normalized.SMTPUsername,
				normalized.SMTPPasswordRef,
				normalized.SMTPTLSMode,
				normalized.IMAPHost,
				normalized.IMAPPort,
				normalized.IMAPUsername,
				normalized.IMAPPasswordRef,
				normalized.IMAPTLSMode,
				existingID,
			)
			if err != nil {
				return nil, fmt.Errorf("reactivating SMTP/IMAP account: %w", err)
			}
			return smtpIMAPAccountResult(existingID, normalized), nil
		}
		return nil, fmt.Errorf("account %s already exists (status: %s)", normalized.Email, existingStatus)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("looking up account: %w", err)
	}

	var id int64
	err = queryRowDB(
		db,
		`INSERT INTO accounts (
			email,
			daily_limit,
			provider,
			gws_config_dir,
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
		) VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		normalized.Email,
		normalized.DailyLimit,
		AccountProviderSMTPIMAP,
		normalized.SMTPHost,
		normalized.SMTPPort,
		normalized.SMTPUsername,
		normalized.SMTPPasswordRef,
		normalized.SMTPTLSMode,
		normalized.IMAPHost,
		normalized.IMAPPort,
		normalized.IMAPUsername,
		normalized.IMAPPasswordRef,
		normalized.IMAPTLSMode,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("adding SMTP/IMAP account: %w", err)
	}

	return smtpIMAPAccountResult(id, normalized), nil
}

func normalizeSMTPIMAPAccountOpts(opts AddSMTPIMAPAccountOpts) (AddSMTPIMAPAccountOpts, error) {
	opts.Email = strings.TrimSpace(opts.Email)
	opts.SMTPHost = strings.TrimSpace(opts.SMTPHost)
	opts.SMTPUsername = strings.TrimSpace(opts.SMTPUsername)
	opts.SMTPPasswordRef = strings.TrimSpace(opts.SMTPPasswordRef)
	opts.SMTPTLSMode = normalizeTLSMode(opts.SMTPTLSMode)
	opts.IMAPHost = strings.TrimSpace(opts.IMAPHost)
	opts.IMAPUsername = strings.TrimSpace(opts.IMAPUsername)
	opts.IMAPPasswordRef = strings.TrimSpace(opts.IMAPPasswordRef)
	opts.IMAPTLSMode = normalizeTLSMode(opts.IMAPTLSMode)

	if opts.Email == "" {
		return opts, fmt.Errorf("email is required")
	}
	if opts.DailyLimit < 1 {
		return opts, fmt.Errorf("daily limit must be at least 1")
	}
	if opts.SMTPHost == "" {
		return opts, fmt.Errorf("smtp host is required")
	}
	if opts.SMTPUsername == "" {
		opts.SMTPUsername = opts.Email
	}
	if opts.SMTPPasswordRef == "" {
		return opts, fmt.Errorf("smtp password ref is required")
	}
	if err := ValidateSecretRef(opts.SMTPPasswordRef); err != nil {
		return opts, fmt.Errorf("smtp password ref: %w", err)
	}
	if err := validateTLSMode("smtp", opts.SMTPTLSMode); err != nil {
		return opts, err
	}
	if opts.SMTPPort == 0 {
		opts.SMTPPort = defaultSMTPPort(opts.SMTPTLSMode)
	}
	if err := validatePort("smtp", opts.SMTPPort); err != nil {
		return opts, err
	}

	if opts.IMAPHost == "" {
		return opts, fmt.Errorf("imap host is required")
	}
	if opts.IMAPUsername == "" {
		opts.IMAPUsername = opts.SMTPUsername
	}
	if opts.IMAPPasswordRef == "" {
		opts.IMAPPasswordRef = opts.SMTPPasswordRef
	}
	if err := ValidateSecretRef(opts.IMAPPasswordRef); err != nil {
		return opts, fmt.Errorf("imap password ref: %w", err)
	}
	if err := validateTLSMode("imap", opts.IMAPTLSMode); err != nil {
		return opts, err
	}
	if opts.IMAPPort == 0 {
		opts.IMAPPort = defaultIMAPPort(opts.IMAPTLSMode)
	}
	if err := validatePort("imap", opts.IMAPPort); err != nil {
		return opts, err
	}

	return opts, nil
}

func smtpIMAPAccountResult(id int64, opts AddSMTPIMAPAccountOpts) *AddSMTPIMAPAccountResult {
	return &AddSMTPIMAPAccountResult{
		ID:           id,
		Email:        opts.Email,
		DailyLimit:   opts.DailyLimit,
		Status:       "active",
		Provider:     AccountProviderSMTPIMAP,
		SMTPHost:     opts.SMTPHost,
		SMTPPort:     opts.SMTPPort,
		SMTPUsername: opts.SMTPUsername,
		SMTPTLSMode:  opts.SMTPTLSMode,
		IMAPHost:     opts.IMAPHost,
		IMAPPort:     opts.IMAPPort,
		IMAPUsername: opts.IMAPUsername,
		IMAPTLSMode:  opts.IMAPTLSMode,
	}
}

func smtpIMAPAccountResultFromAccount(account Account) *AddSMTPIMAPAccountResult {
	return &AddSMTPIMAPAccountResult{
		ID:           account.ID,
		Email:        account.Email,
		DailyLimit:   account.DailyLimit,
		Status:       account.Status,
		Provider:     account.Provider,
		SMTPHost:     account.SMTPHost,
		SMTPPort:     account.SMTPPort,
		SMTPUsername: account.SMTPUsername,
		SMTPTLSMode:  account.SMTPTLSMode,
		IMAPHost:     account.IMAPHost,
		IMAPPort:     account.IMAPPort,
		IMAPUsername: account.IMAPUsername,
		IMAPTLSMode:  account.IMAPTLSMode,
	}
}

func normalizeTLSMode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "ssl"
	}
	return value
}

func validateTLSMode(label, value string) error {
	switch value {
	case "ssl", "starttls", "none":
		return nil
	default:
		return fmt.Errorf("%s tls mode must be one of: ssl, starttls, none", label)
	}
}

func defaultSMTPPort(tlsMode string) int {
	switch tlsMode {
	case "starttls":
		return 587
	case "none":
		return 25
	default:
		return 465
	}
}

func defaultIMAPPort(tlsMode string) int {
	if tlsMode == "ssl" {
		return 993
	}
	return 143
}

func validatePort(label string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s port must be between 1 and 65535", label)
	}
	return nil
}

func accountSelectColumns() string {
	return `id,
		email,
		daily_limit,
		status,
		provider,
		gws_config_dir,
		smtp_host,
		smtp_port,
		smtp_username,
		smtp_password_ref,
		smtp_tls_mode,
		imap_host,
		imap_port,
		imap_username,
		imap_password_ref,
		imap_tls_mode`
}

func scanAccount(row interface {
	Scan(dest ...any) error
}) (Account, error) {
	var account Account
	if err := row.Scan(
		&account.ID,
		&account.Email,
		&account.DailyLimit,
		&account.Status,
		&account.Provider,
		&account.GWSConfigDir,
		&account.SMTPHost,
		&account.SMTPPort,
		&account.SMTPUsername,
		&account.SMTPPasswordRef,
		&account.SMTPTLSMode,
		&account.IMAPHost,
		&account.IMAPPort,
		&account.IMAPUsername,
		&account.IMAPPasswordRef,
		&account.IMAPTLSMode,
	); err != nil {
		return Account{}, err
	}
	return account, nil
}

// GetAccountByEmail loads a full account record by email.
func GetAccountByEmail(db *sql.DB, email string) (Account, error) {
	email = strings.TrimSpace(email)
	account, err := scanAccount(queryRowDB(db, "SELECT "+accountSelectColumns()+" FROM accounts WHERE email = ?", email))
	if err == sql.ErrNoRows {
		return Account{}, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return Account{}, fmt.Errorf("loading account %s: %w", email, err)
	}
	return account, nil
}

// UpdateSMTPIMAPAccount modifies provider settings on a generic SMTP/IMAP account.
func UpdateSMTPIMAPAccount(db *sql.DB, email string, opts UpdateSMTPIMAPAccountOpts) (*AddSMTPIMAPAccountResult, error) {
	account, err := GetAccountByEmail(db, email)
	if err != nil {
		return nil, err
	}
	if account.Status == "removed" {
		return nil, fmt.Errorf("account %s has been removed — re-add it first", email)
	}
	if account.Provider != AccountProviderSMTPIMAP {
		return nil, fmt.Errorf("account %s is provider %s, expected %s", email, account.Provider, AccountProviderSMTPIMAP)
	}

	merged := AddSMTPIMAPAccountOpts{
		Email:           account.Email,
		DailyLimit:      account.DailyLimit,
		SMTPHost:        account.SMTPHost,
		SMTPPort:        account.SMTPPort,
		SMTPUsername:    account.SMTPUsername,
		SMTPPasswordRef: account.SMTPPasswordRef,
		SMTPTLSMode:     account.SMTPTLSMode,
		IMAPHost:        account.IMAPHost,
		IMAPPort:        account.IMAPPort,
		IMAPUsername:    account.IMAPUsername,
		IMAPPasswordRef: account.IMAPPasswordRef,
		IMAPTLSMode:     account.IMAPTLSMode,
	}

	if opts.DailyLimit != nil {
		merged.DailyLimit = *opts.DailyLimit
	}
	if opts.SMTPHost != nil {
		merged.SMTPHost = *opts.SMTPHost
	}
	if opts.SMTPPort != nil {
		merged.SMTPPort = *opts.SMTPPort
	}
	if opts.SMTPUsername != nil {
		merged.SMTPUsername = *opts.SMTPUsername
	}
	if opts.SMTPPasswordRef != nil {
		merged.SMTPPasswordRef = *opts.SMTPPasswordRef
	}
	if opts.SMTPTLSMode != nil {
		merged.SMTPTLSMode = *opts.SMTPTLSMode
	}
	if opts.IMAPHost != nil {
		merged.IMAPHost = *opts.IMAPHost
	}
	if opts.IMAPPort != nil {
		merged.IMAPPort = *opts.IMAPPort
	}
	if opts.IMAPUsername != nil {
		merged.IMAPUsername = *opts.IMAPUsername
	}
	if opts.IMAPPasswordRef != nil {
		merged.IMAPPasswordRef = *opts.IMAPPasswordRef
	}
	if opts.IMAPTLSMode != nil {
		merged.IMAPTLSMode = *opts.IMAPTLSMode
	}

	normalized, err := normalizeSMTPIMAPAccountOpts(merged)
	if err != nil {
		return nil, err
	}

	tx, err := beginTx(db)
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE accounts
		 SET daily_limit = ?,
		     smtp_host = ?,
		     smtp_port = ?,
		     smtp_username = ?,
		     smtp_password_ref = ?,
		     smtp_tls_mode = ?,
		     imap_host = ?,
		     imap_port = ?,
		     imap_username = ?,
		     imap_password_ref = ?,
		     imap_tls_mode = ?
		 WHERE id = ?`,
		normalized.DailyLimit,
		normalized.SMTPHost,
		normalized.SMTPPort,
		normalized.SMTPUsername,
		normalized.SMTPPasswordRef,
		normalized.SMTPTLSMode,
		normalized.IMAPHost,
		normalized.IMAPPort,
		normalized.IMAPUsername,
		normalized.IMAPPasswordRef,
		normalized.IMAPTLSMode,
		account.ID,
	); err != nil {
		return nil, fmt.Errorf("updating SMTP/IMAP account: %w", err)
	}
	if opts.DailyLimit != nil {
		if err := rebalancePendingSchedulesTx(tx, []int64{account.ID}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	updated, err := GetAccountByEmail(db, account.Email)
	if err != nil {
		return nil, err
	}
	return smtpIMAPAccountResultFromAccount(updated), nil
}

// PauseAccountResult is returned by PauseAccount.
type PauseAccountResult struct {
	Email          string `json:"email"`
	CancelledSends int64  `json:"cancelled_sends"`
}

// PauseAccount deactivates an account and cancels its pending sends.
func PauseAccount(db *sql.DB, email string) (*PauseAccountResult, error) {
	var id int64
	var status string
	err := queryRowDB(db, "SELECT id, status FROM accounts WHERE email = ?", email).Scan(&id, &status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up account: %w", err)
	}
	if status != "active" {
		return nil, fmt.Errorf("account %s is already %s", email, status)
	}

	tx, err := beginTx(db)
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	tx.Exec("UPDATE accounts SET status = 'paused' WHERE id = ?", id)

	res, _ := tx.Exec("UPDATE scheduled_sends SET status = 'cancelled' WHERE account_id = ? AND status = 'pending'", id)
	cancelled, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &PauseAccountResult{Email: email, CancelledSends: cancelled}, nil
}

// ResumeAccountResult is returned by ResumeAccount.
type ResumeAccountResult struct {
	Email         string `json:"email"`
	RestoredSends int64  `json:"restored_sends"`
}

// ResumeAccount reactivates a paused account and restores its eligible sends.
func ResumeAccount(db *sql.DB, email string) (*ResumeAccountResult, error) {
	var id int64
	var status string
	err := queryRowDB(db, "SELECT id, status FROM accounts WHERE email = ?", email).Scan(&id, &status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up account: %w", err)
	}
	if status != "paused" {
		return nil, fmt.Errorf("account %s is %s (expected paused)", email, status)
	}

	tx, err := beginTx(db)
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("UPDATE accounts SET status = 'active' WHERE id = ?", id); err != nil {
		return nil, fmt.Errorf("resuming account: %w", err)
	}

	res, err := tx.Exec(`
		UPDATE scheduled_sends SET status = 'pending'
		WHERE account_id = ? AND status = 'cancelled'
		AND campaign_id IN (SELECT id FROM campaigns WHERE status IN ('active', 'draft'))
		AND EXISTS (
			SELECT 1
			FROM campaign_leads cl
			JOIN leads l ON l.id = cl.lead_id
			WHERE cl.campaign_id = scheduled_sends.campaign_id
			  AND cl.lead_id = scheduled_sends.lead_id
			  AND cl.status = 'active'
			  AND l.global_status = 'active'
		)`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("restoring sends: %w", err)
	}
	restored, _ := res.RowsAffected()

	if err := rebalancePendingSchedulesTx(tx, []int64{id}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &ResumeAccountResult{Email: email, RestoredSends: restored}, nil
}

// RemoveAccount deactivates an account permanently and cancels its pending sends.
// The account row is kept (status='removed') because historical sends/events reference it.
func RemoveAccount(db *sql.DB, email string) (*PauseAccountResult, error) {
	var id int64
	err := queryRowDB(db, "SELECT id FROM accounts WHERE email = ?", email).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up account: %w", err)
	}

	tx, err := beginTx(db)
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	res, _ := tx.Exec("UPDATE scheduled_sends SET status = 'cancelled' WHERE account_id = ? AND status = 'pending'", id)
	cancelled, _ := res.RowsAffected()

	tx.Exec("DELETE FROM campaign_accounts WHERE account_id = ?", id)
	tx.Exec("UPDATE accounts SET status = 'removed' WHERE id = ?", id)

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &PauseAccountResult{Email: email, CancelledSends: cancelled}, nil
}

// UpdateAccountOpts holds fields to update on an account.
type UpdateAccountOpts struct {
	DailyLimit *int
}

// UpdateAccount modifies account settings.
func UpdateAccount(db *sql.DB, email string, opts UpdateAccountOpts) error {
	var id int64
	var status string
	err := queryRowDB(db, "SELECT id, status FROM accounts WHERE email = ?", email).Scan(&id, &status)
	if err != nil {
		return fmt.Errorf("account %s not found", email)
	}
	if status == "removed" {
		return fmt.Errorf("account %s has been removed — re-add it first", email)
	}

	if opts.DailyLimit != nil {
		if *opts.DailyLimit < 1 {
			return fmt.Errorf("daily limit must be at least 1")
		}
		tx, err := beginTx(db)
		if err != nil {
			return fmt.Errorf("starting transaction: %w", err)
		}
		defer tx.Rollback()

		if _, err := tx.Exec("UPDATE accounts SET daily_limit = ? WHERE id = ?", *opts.DailyLimit, id); err != nil {
			return fmt.Errorf("updating daily limit: %w", err)
		}
		if err := rebalancePendingSchedulesTx(tx, []int64{id}); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing: %w", err)
		}
	}
	return nil
}

// DomainCheck is the result of checking one DNS aspect.
type DomainCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// DomainDiagnostic is the full result of CheckDomain.
type DomainDiagnostic struct {
	Domain   string        `json:"domain"`
	Checks   []DomainCheck `json:"checks"`
	Score    int           `json:"score"`
	MaxScore int           `json:"max_score"`
}

// CheckDomain runs DNS diagnostics for email deliverability.
func CheckDomain(domain string) (*DomainDiagnostic, error) {
	diag := &DomainDiagnostic{
		Domain:   domain,
		MaxScore: 4,
	}

	// 1. MX records
	mxRecords, err := net.LookupMX(domain)
	if err != nil || len(mxRecords) == 0 {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "MX",
			Passed: false,
			Detail: "No MX records found",
			Fix:    "Add MX records pointing to your email provider",
		})
	} else {
		hosts := make([]string, len(mxRecords))
		for i, mx := range mxRecords {
			hosts[i] = strings.TrimSuffix(mx.Host, ".")
		}
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "MX",
			Passed: true,
			Detail: fmt.Sprintf("%d records: %s", len(mxRecords), strings.Join(hosts, ", ")),
		})
		diag.Score++
	}

	// 2. SPF record
	spfFound := false
	txtRecords, _ := net.LookupTXT(domain)
	for _, txt := range txtRecords {
		if strings.HasPrefix(txt, "v=spf1") {
			spfFound = true
			diag.Checks = append(diag.Checks, DomainCheck{
				Name:   "SPF",
				Passed: true,
				Detail: txt,
			})
			break
		}
	}
	if !spfFound {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "SPF",
			Passed: false,
			Detail: "No SPF record found",
			Fix:    fmt.Sprintf("Add TXT record to %s: v=spf1 include:_spf.google.com ~all", domain),
		})
	} else {
		diag.Score++
	}

	// 3. DKIM (check common selectors)
	dkimSelectors := []string{"google", "default", "selector1", "selector2", "k1", "k2", "k3", "key1", "key2", "key3", "dkim", "mail", "cloudflare", "migadu", "protonmail", "zoho", "s1", "s2", "smtp"}
	dkimFound := false
	dkimSelector := ""
	for _, sel := range dkimSelectors {
		dkimDomain := sel + "._domainkey." + domain
		dkimRecords, _ := net.LookupTXT(dkimDomain)
		for _, txt := range dkimRecords {
			if strings.Contains(txt, "v=DKIM1") || strings.Contains(txt, "k=rsa") || strings.Contains(txt, "p=") {
				dkimFound = true
				dkimSelector = dkimDomain
				break
			}
		}
		if dkimFound {
			break
		}
	}
	if dkimFound {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "DKIM",
			Passed: true,
			Detail: fmt.Sprintf("Found at %s", dkimSelector),
		})
		diag.Score++
	} else {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "DKIM",
			Passed: false,
			Detail: "No DKIM record found (checked: " + strings.Join(dkimSelectors, ", ") + ")",
			Fix:    "Set up DKIM signing with your email provider",
		})
	}

	// 4. DMARC
	dmarcDomain := "_dmarc." + domain
	dmarcRecords, _ := net.LookupTXT(dmarcDomain)
	dmarcFound := false
	for _, txt := range dmarcRecords {
		if strings.HasPrefix(txt, "v=DMARC1") {
			dmarcFound = true
			diag.Checks = append(diag.Checks, DomainCheck{
				Name:   "DMARC",
				Passed: true,
				Detail: txt,
			})
			break
		}
	}
	if !dmarcFound {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "DMARC",
			Passed: false,
			Detail: "No DMARC policy found",
			Fix:    fmt.Sprintf("Add TXT record to %s: v=DMARC1; p=none; rua=mailto:dmarc@%s", dmarcDomain, domain),
		})
	} else {
		diag.Score++
	}

	// 5. Domain age via WHOIS
	diag.MaxScore = 5
	whoisRaw, err := whois.Whois(domain)
	if err == nil {
		parsed, err := whoisparser.Parse(whoisRaw)
		if err == nil && parsed.Domain.CreatedDate != "" {
			// Try common date formats
			var created time.Time
			for _, layout := range []string{
				time.RFC3339,
				"2006-01-02T15:04:05Z",
				"2006-01-02T15:04:05-07:00",
				"2006-01-02",
				"02-Jan-2006",
				"2006-01-02 15:04:05",
			} {
				if t, err := time.Parse(layout, parsed.Domain.CreatedDate); err == nil {
					created = t
					break
				}
			}

			if !created.IsZero() {
				years := time.Since(created).Hours() / 24 / 365.25
				years = math.Round(years*10) / 10
				if years >= 1 {
					diag.Checks = append(diag.Checks, DomainCheck{
						Name:   "Age",
						Passed: true,
						Detail: fmt.Sprintf("%.1f years (good)", years),
					})
					diag.Score++
				} else if years >= 0.25 {
					diag.Checks = append(diag.Checks, DomainCheck{
						Name:   "Age",
						Passed: true,
						Detail: fmt.Sprintf("%.1f years (ok — warm up slowly)", years),
					})
					diag.Score++
				} else {
					diag.Checks = append(diag.Checks, DomainCheck{
						Name:   "Age",
						Passed: false,
						Detail: fmt.Sprintf("%.1f years — very new domain, high spam risk", years),
						Fix:    "New domains need 2-4 weeks of warmup before cold outreach",
					})
				}
			} else {
				diag.Checks = append(diag.Checks, DomainCheck{
					Name:   "Age",
					Passed: true,
					Detail: "Could not parse creation date",
				})
				diag.Score++
			}
		} else {
			diag.Checks = append(diag.Checks, DomainCheck{
				Name:   "Age",
				Passed: true,
				Detail: "WHOIS data unavailable",
			})
			diag.Score++
		}
	} else {
		diag.Checks = append(diag.Checks, DomainCheck{
			Name:   "Age",
			Passed: true,
			Detail: "WHOIS lookup failed",
		})
		diag.Score++
	}

	return diag, nil
}

// ListAccountsRow is a row from ListAccounts.
type ListAccountsRow struct {
	ID         int64  `json:"id"`
	Email      string `json:"email"`
	DailyLimit int    `json:"daily_limit"`
	Status     string `json:"status"`
	Provider   string `json:"provider"`
}

// ListAccounts returns all accounts ordered by ID.
func ListAccounts(db *sql.DB) ([]ListAccountsRow, error) {
	rows, err := queryDB(db, "SELECT id, email, daily_limit, status, provider FROM accounts ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("querying accounts: %w", err)
	}
	defer rows.Close()

	var accounts []ListAccountsRow
	for rows.Next() {
		var a ListAccountsRow
		if err := rows.Scan(&a.ID, &a.Email, &a.DailyLimit, &a.Status, &a.Provider); err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}
