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
	GWSConfigDir string `json:"gws_config_dir"`
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
			_, err := execDB(db, "UPDATE accounts SET status = 'active', daily_limit = ?, gws_config_dir = ? WHERE id = ?",
				dailyLimit, configDir, existingID)
			if err != nil {
				return nil, fmt.Errorf("reactivating account: %w", err)
			}
			return &AddAccountResult{
				ID:           existingID,
				Email:        email,
				DailyLimit:   dailyLimit,
				Status:       "active",
				GWSConfigDir: configDir,
			}, nil
		}
		return nil, fmt.Errorf("account %s already exists (status: %s)", email, existingStatus)
	}

	var id int64
	err = queryRowDB(
		db,
		"INSERT INTO accounts (email, daily_limit, gws_config_dir) VALUES (?, ?, ?) RETURNING id",
		email, dailyLimit, configDir,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("adding account: %w", err)
	}
	return &AddAccountResult{
		ID:           id,
		Email:        email,
		DailyLimit:   dailyLimit,
		Status:       "active",
		GWSConfigDir: configDir,
	}, nil
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

// ResumeAccount reactivates a paused account.
func ResumeAccount(db *sql.DB, email string) error {
	var status string
	err := queryRowDB(db, "SELECT status FROM accounts WHERE email = ?", email).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return fmt.Errorf("looking up account: %w", err)
	}
	if status != "paused" {
		return fmt.Errorf("account %s is %s (expected paused)", email, status)
	}

	_, err = execDB(db, "UPDATE accounts SET status = 'active' WHERE email = ?", email)
	return err
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
}

// ListAccounts returns all accounts ordered by ID.
func ListAccounts(db *sql.DB) ([]ListAccountsRow, error) {
	rows, err := queryDB(db, "SELECT id, email, daily_limit, status FROM accounts ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("querying accounts: %w", err)
	}
	defer rows.Close()

	var accounts []ListAccountsRow
	for rows.Next() {
		var a ListAccountsRow
		if err := rows.Scan(&a.ID, &a.Email, &a.DailyLimit, &a.Status); err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}
