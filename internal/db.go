package internal

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	daily_limit INTEGER NOT NULL DEFAULT 50,
	last_send_at DATETIME,
	status TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS campaigns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL DEFAULT 'draft',
	sequence_file TEXT NOT NULL,
	stop_on_reply INTEGER NOT NULL DEFAULT 1,
	stop_on_domain_reply INTEGER NOT NULL DEFAULT 0,
	send_window_start TEXT NOT NULL DEFAULT '09:00',
	send_window_end TEXT NOT NULL DEFAULT '17:00',
	send_days TEXT NOT NULL DEFAULT '1,2,3,4,5',
	timezone TEXT NOT NULL DEFAULT 'America/New_York',
	min_gap_seconds INTEGER NOT NULL DEFAULT 90,
	max_gap_seconds INTEGER NOT NULL DEFAULT 140,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS campaign_accounts (
	campaign_id INTEGER NOT NULL REFERENCES campaigns(id),
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	PRIMARY KEY (campaign_id, account_id)
);

CREATE TABLE IF NOT EXISTS leads (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	first_name TEXT NOT NULL DEFAULT '',
	last_name TEXT NOT NULL DEFAULT '',
	company TEXT NOT NULL DEFAULT '',
	domain TEXT NOT NULL DEFAULT '',
	custom_fields TEXT NOT NULL DEFAULT '{}',
	global_status TEXT NOT NULL DEFAULT 'active',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS campaign_leads (
	campaign_id INTEGER NOT NULL REFERENCES campaigns(id),
	lead_id INTEGER NOT NULL REFERENCES leads(id),
	status TEXT NOT NULL DEFAULT 'active',
	started_at DATETIME,
	PRIMARY KEY (campaign_id, lead_id)
);

CREATE TABLE IF NOT EXISTS scheduled_sends (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	campaign_id INTEGER NOT NULL REFERENCES campaigns(id),
	lead_id INTEGER NOT NULL REFERENCES leads(id),
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	step_number INTEGER NOT NULL,
	variant_index INTEGER NOT NULL DEFAULT 0,
	send_at DATETIME NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	thread_id TEXT NOT NULL DEFAULT '',
	parent_message_id TEXT NOT NULL DEFAULT '',
	message_id TEXT NOT NULL DEFAULT '',
	sent_at DATETIME
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	campaign_id INTEGER NOT NULL REFERENCES campaigns(id),
	lead_id INTEGER NOT NULL REFERENCES leads(id),
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	type TEXT NOT NULL,
	step_number INTEGER NOT NULL,
	message_id TEXT NOT NULL DEFAULT '',
	thread_id TEXT NOT NULL DEFAULT '',
	timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	metadata TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_sends_pending ON scheduled_sends(status, send_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_events_account_day ON events(account_id, type, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_message_id ON events(message_id);
CREATE INDEX IF NOT EXISTS idx_leads_email ON leads(email);
CREATE INDEX IF NOT EXISTS idx_leads_domain ON leads(domain);
`

// DataDir returns the cold-cli data directory path.
func DataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cold-cli")
}

// DBPath returns the database file path.
func DBPath() string {
	return filepath.Join(DataDir(), "data.db")
}

// EnsureDataDir creates the data directory if it doesn't exist.
func EnsureDataDir() error {
	return os.MkdirAll(DataDir(), 0755)
}

// OpenDB opens (or creates) the SQLite database and runs migrations.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Enable WAL mode and foreign keys
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting %s: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("running schema migration: %w", err)
	}

	return db, nil
}
