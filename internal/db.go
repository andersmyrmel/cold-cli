package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	daily_limit INTEGER NOT NULL DEFAULT 50,
	last_send_at DATETIME,
	status TEXT NOT NULL DEFAULT 'active',
	provider TEXT NOT NULL DEFAULT 'gws',
	gws_config_dir TEXT NOT NULL DEFAULT '',
	smtp_host TEXT NOT NULL DEFAULT '',
	smtp_port INTEGER NOT NULL DEFAULT 0,
	smtp_username TEXT NOT NULL DEFAULT '',
	smtp_password_ref TEXT NOT NULL DEFAULT '',
	smtp_tls_mode TEXT NOT NULL DEFAULT '',
	imap_host TEXT NOT NULL DEFAULT '',
	imap_port INTEGER NOT NULL DEFAULT 0,
	imap_username TEXT NOT NULL DEFAULT '',
	imap_password_ref TEXT NOT NULL DEFAULT '',
	imap_tls_mode TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS campaigns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL DEFAULT 'draft',
	sequence_file TEXT NOT NULL,
	sequence_content TEXT NOT NULL DEFAULT '',
	start_date TEXT NOT NULL DEFAULT '',
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
	sent_at DATETIME,
	error_message TEXT NOT NULL DEFAULT ''
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

CREATE TABLE IF NOT EXISTS email_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	campaign_id INTEGER NOT NULL REFERENCES campaigns(id),
	lead_id INTEGER NOT NULL REFERENCES leads(id),
	account_id INTEGER NOT NULL REFERENCES accounts(id),
	direction TEXT NOT NULL,
	type TEXT NOT NULL,
	step_number INTEGER NOT NULL DEFAULT 0,
	scheduled_send_id INTEGER REFERENCES scheduled_sends(id),
	event_id INTEGER REFERENCES events(id),
	message_id TEXT NOT NULL DEFAULT '',
	thread_id TEXT NOT NULL DEFAULT '',
	in_reply_to TEXT NOT NULL DEFAULT '',
	from_email TEXT NOT NULL DEFAULT '',
	to_emails TEXT NOT NULL DEFAULT '',
	subject TEXT NOT NULL DEFAULT '',
	text_body TEXT NOT NULL DEFAULT '',
	display_body TEXT NOT NULL DEFAULT '',
	display_html TEXT NOT NULL DEFAULT '',
	html_body TEXT NOT NULL DEFAULT '',
	snippet TEXT NOT NULL DEFAULT '',
	raw_headers TEXT NOT NULL DEFAULT '{}',
	occurred_at DATETIME NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sends_pending ON scheduled_sends(status, send_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_events_account_day ON events(account_id, type, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_message_id ON events(message_id);
CREATE INDEX IF NOT EXISTS idx_events_thread_id ON events(thread_id);
CREATE INDEX IF NOT EXISTS idx_email_messages_thread ON email_messages(campaign_id, lead_id, thread_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_email_messages_message_id ON email_messages(message_id);
CREATE INDEX IF NOT EXISTS idx_email_messages_account ON email_messages(account_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_leads_email ON leads(email);
CREATE INDEX IF NOT EXISTS idx_leads_domain ON leads(domain);
`

var postgresSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS accounts (
		id BIGSERIAL PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		daily_limit INTEGER NOT NULL DEFAULT 50,
		last_send_at TIMESTAMPTZ,
		status TEXT NOT NULL DEFAULT 'active',
		provider TEXT NOT NULL DEFAULT 'gws',
		gws_config_dir TEXT NOT NULL DEFAULT '',
		smtp_host TEXT NOT NULL DEFAULT '',
		smtp_port INTEGER NOT NULL DEFAULT 0,
		smtp_username TEXT NOT NULL DEFAULT '',
		smtp_password_ref TEXT NOT NULL DEFAULT '',
		smtp_tls_mode TEXT NOT NULL DEFAULT '',
		imap_host TEXT NOT NULL DEFAULT '',
		imap_port INTEGER NOT NULL DEFAULT 0,
		imap_username TEXT NOT NULL DEFAULT '',
		imap_password_ref TEXT NOT NULL DEFAULT '',
		imap_tls_mode TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS campaigns (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'draft',
		sequence_file TEXT NOT NULL,
		sequence_content TEXT NOT NULL DEFAULT '',
		start_date TEXT NOT NULL DEFAULT '',
		stop_on_reply INTEGER NOT NULL DEFAULT 1,
		stop_on_domain_reply INTEGER NOT NULL DEFAULT 0,
		send_window_start TEXT NOT NULL DEFAULT '09:00',
		send_window_end TEXT NOT NULL DEFAULT '17:00',
		send_days TEXT NOT NULL DEFAULT '1,2,3,4,5',
		timezone TEXT NOT NULL DEFAULT 'America/New_York',
		min_gap_seconds INTEGER NOT NULL DEFAULT 90,
		max_gap_seconds INTEGER NOT NULL DEFAULT 140,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS campaign_accounts (
		campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
		account_id BIGINT NOT NULL REFERENCES accounts(id),
		PRIMARY KEY (campaign_id, account_id)
	)`,
	`CREATE TABLE IF NOT EXISTS leads (
		id BIGSERIAL PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		first_name TEXT NOT NULL DEFAULT '',
		last_name TEXT NOT NULL DEFAULT '',
		company TEXT NOT NULL DEFAULT '',
		domain TEXT NOT NULL DEFAULT '',
		custom_fields TEXT NOT NULL DEFAULT '{}',
		global_status TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS campaign_leads (
		campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
		lead_id BIGINT NOT NULL REFERENCES leads(id),
		status TEXT NOT NULL DEFAULT 'active',
		started_at TIMESTAMPTZ,
		PRIMARY KEY (campaign_id, lead_id)
	)`,
	`CREATE TABLE IF NOT EXISTS scheduled_sends (
		id BIGSERIAL PRIMARY KEY,
		campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
		lead_id BIGINT NOT NULL REFERENCES leads(id),
		account_id BIGINT NOT NULL REFERENCES accounts(id),
		step_number INTEGER NOT NULL,
		variant_index INTEGER NOT NULL DEFAULT 0,
		send_at TIMESTAMPTZ NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		thread_id TEXT NOT NULL DEFAULT '',
		parent_message_id TEXT NOT NULL DEFAULT '',
		message_id TEXT NOT NULL DEFAULT '',
		sent_at TIMESTAMPTZ,
		error_message TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id BIGSERIAL PRIMARY KEY,
		campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
		lead_id BIGINT NOT NULL REFERENCES leads(id),
		account_id BIGINT NOT NULL REFERENCES accounts(id),
		type TEXT NOT NULL,
		step_number INTEGER NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL DEFAULT '',
		timestamp TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		metadata TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE TABLE IF NOT EXISTS email_messages (
		id BIGSERIAL PRIMARY KEY,
		campaign_id BIGINT NOT NULL REFERENCES campaigns(id),
		lead_id BIGINT NOT NULL REFERENCES leads(id),
		account_id BIGINT NOT NULL REFERENCES accounts(id),
		direction TEXT NOT NULL,
		type TEXT NOT NULL,
		step_number INTEGER NOT NULL DEFAULT 0,
		scheduled_send_id BIGINT REFERENCES scheduled_sends(id),
		event_id BIGINT REFERENCES events(id),
		message_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL DEFAULT '',
		in_reply_to TEXT NOT NULL DEFAULT '',
		from_email TEXT NOT NULL DEFAULT '',
		to_emails TEXT NOT NULL DEFAULT '',
		subject TEXT NOT NULL DEFAULT '',
		text_body TEXT NOT NULL DEFAULT '',
		display_body TEXT NOT NULL DEFAULT '',
		display_html TEXT NOT NULL DEFAULT '',
		html_body TEXT NOT NULL DEFAULT '',
		snippet TEXT NOT NULL DEFAULT '',
		raw_headers TEXT NOT NULL DEFAULT '{}',
		occurred_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS kv (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_sends_pending ON scheduled_sends(status, send_at) WHERE status = 'pending'`,
	`CREATE INDEX IF NOT EXISTS idx_events_account_day ON events(account_id, type, timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_events_message_id ON events(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_thread_id ON events(thread_id)`,
	`CREATE INDEX IF NOT EXISTS idx_email_messages_thread ON email_messages(campaign_id, lead_id, thread_id, occurred_at)`,
	`CREATE INDEX IF NOT EXISTS idx_email_messages_message_id ON email_messages(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_email_messages_account ON email_messages(account_id, occurred_at)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_email ON leads(email)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_domain ON leads(domain)`,
}

var postgresMigrationStatements = []string{
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS gws_config_dir TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'gws'`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS smtp_host TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS smtp_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS smtp_username TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS smtp_password_ref TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS smtp_tls_mode TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS imap_host TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS imap_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS imap_username TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS imap_password_ref TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN IF NOT EXISTS imap_tls_mode TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE email_messages ADD COLUMN IF NOT EXISTS display_body TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE email_messages ADD COLUMN IF NOT EXISTS display_html TEXT NOT NULL DEFAULT ''`,
}

const (
	sqliteBusyTimeoutMS  = 5000
	sqliteWriteAttempts  = 5
	sqliteRetryBaseDelay = 50 * time.Millisecond
)

// DataDir returns the cold-cli data directory path.
// Respects COLD_CLI_DATA_DIR env var for testing.
func DataDir() string {
	if dir := os.Getenv("COLD_CLI_DATA_DIR"); dir != "" {
		return dir
	}
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
	store, err := openStore(storeOpenConfig{
		dialect:    DialectSQLite,
		sqlitePath: path,
	})
	if err != nil {
		return nil, err
	}
	return store.DB, nil
}

func bootstrapSQLiteSchema(db *sql.DB) error {
	if err := withBusyRetry(func() error {
		_, err := db.Exec(schema)
		return err
	}); err != nil {
		return fmt.Errorf("running schema migration: %w", err)
	}

	if err := runSQLiteMigrations(db); err != nil {
		return err
	}
	if err := backfillEmailMessageDisplayBodies(db); err != nil {
		return err
	}

	return nil
}

func bootstrapPostgresSchema(db *sql.DB) error {
	for _, stmt := range postgresSchemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("running postgres schema statement %q: %w", stmt, err)
		}
	}
	for _, stmt := range postgresMigrationStatements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("running postgres migration statement %q: %w", stmt, err)
		}
	}
	if err := backfillEmailMessageDisplayBodies(db); err != nil {
		return err
	}
	return nil
}

// runSQLiteMigrations applies incremental schema changes to existing databases.
func runSQLiteMigrations(db *sql.DB) error {
	migrations := []string{
		"ALTER TABLE accounts ADD COLUMN gws_config_dir TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT 'gws'",
		"ALTER TABLE accounts ADD COLUMN smtp_host TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN smtp_port INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE accounts ADD COLUMN smtp_username TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN smtp_password_ref TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN smtp_tls_mode TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN imap_host TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN imap_port INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE accounts ADD COLUMN imap_username TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN imap_password_ref TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE accounts ADD COLUMN imap_tls_mode TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE email_messages ADD COLUMN display_body TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE email_messages ADD COLUMN display_html TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE campaigns ADD COLUMN sequence_content TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE campaigns ADD COLUMN start_date TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE scheduled_sends ADD COLUMN error_message TEXT NOT NULL DEFAULT ''",
	}
	for _, m := range migrations {
		if err := withBusyRetry(func() error {
			_, err := db.Exec(m)
			return err
		}); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return fmt.Errorf("running migration %q: %w", m, err)
		}
	}
	return nil
}

func sqliteDSN(path string) string {
	q := url.Values{}
	q.Add("_txlock", "immediate")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMS))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")

	if path == ":memory:" {
		u := &url.URL{Scheme: "file", Opaque: ":memory:"}
		u.RawQuery = q.Encode()
		return u.String()
	}

	q.Add("_pragma", "journal_mode(WAL)")

	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + q.Encode()
	}

	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = q.Encode()
	return u.String()
}

func withBusyRetry(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < sqliteWriteAttempts; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if !isSQLiteBusyError(err) || attempt == sqliteWriteAttempts-1 {
				return err
			}
			time.Sleep(sqliteRetryDelay(attempt))
			continue
		}
		return nil
	}
	return lastErr
}

func withRetryTx[T any](db *sql.DB, fn func(tx *Tx) (T, error)) (T, error) {
	var zero T
	var lastErr error

	for attempt := 0; attempt < sqliteWriteAttempts; attempt++ {
		tx, err := beginTx(db)
		if err != nil {
			lastErr = fmt.Errorf("starting transaction: %w", err)
			if !isSQLiteBusyError(lastErr) || attempt == sqliteWriteAttempts-1 {
				return zero, lastErr
			}
			time.Sleep(sqliteRetryDelay(attempt))
			continue
		}

		result, err := fn(tx)
		if err != nil {
			_ = tx.Rollback()
			lastErr = err
			if !isSQLiteBusyError(err) || attempt == sqliteWriteAttempts-1 {
				return zero, err
			}
			time.Sleep(sqliteRetryDelay(attempt))
			continue
		}

		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			lastErr = fmt.Errorf("committing: %w", err)
			if !isSQLiteBusyError(lastErr) || attempt == sqliteWriteAttempts-1 {
				return zero, lastErr
			}
			time.Sleep(sqliteRetryDelay(attempt))
			continue
		}

		return result, nil
	}

	return zero, lastErr
}

func isSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case 5, 6:
			return true
		}
	}

	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "sqlite_busy") || strings.Contains(lower, "database is locked")
}

func sqliteRetryDelay(attempt int) time.Duration {
	return sqliteRetryBaseDelay * time.Duration(1<<attempt)
}
