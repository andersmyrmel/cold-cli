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
