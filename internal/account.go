package internal

import (
	"database/sql"
	"fmt"
	"strings"
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
func AddAccount(db *sql.DB, email string, dailyLimit int, configDir string) (*AddAccountResult, error) {
	result, err := db.Exec(
		"INSERT INTO accounts (email, daily_limit, gws_config_dir) VALUES (?, ?, ?)",
		email, dailyLimit, configDir,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("account %s already exists", email)
		}
		return nil, fmt.Errorf("adding account: %w", err)
	}

	id, _ := result.LastInsertId()
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
	err := db.QueryRow("SELECT id, status FROM accounts WHERE email = ?", email).Scan(&id, &status)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up account: %w", err)
	}
	if status != "active" {
		return nil, fmt.Errorf("account %s is already %s", email, status)
	}

	tx, err := db.Begin()
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
	err := db.QueryRow("SELECT status FROM accounts WHERE email = ?", email).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return fmt.Errorf("looking up account: %w", err)
	}
	if status != "paused" {
		return fmt.Errorf("account %s is %s (expected paused)", email, status)
	}

	_, err = db.Exec("UPDATE accounts SET status = 'active' WHERE email = ?", email)
	return err
}

// RemoveAccount deactivates an account permanently and cancels its pending sends.
// The account row is kept (status='removed') because historical sends/events reference it.
func RemoveAccount(db *sql.DB, email string) (*PauseAccountResult, error) {
	var id int64
	err := db.QueryRow("SELECT id FROM accounts WHERE email = ?", email).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("account %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up account: %w", err)
	}

	tx, err := db.Begin()
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

// ListAccountsRow is a row from ListAccounts.
type ListAccountsRow struct {
	ID         int64  `json:"id"`
	Email      string `json:"email"`
	DailyLimit int    `json:"daily_limit"`
	Status     string `json:"status"`
}

// ListAccounts returns all accounts ordered by ID.
func ListAccounts(db *sql.DB) ([]ListAccountsRow, error) {
	rows, err := db.Query("SELECT id, email, daily_limit, status FROM accounts ORDER BY id")
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
