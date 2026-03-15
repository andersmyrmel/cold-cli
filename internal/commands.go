package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
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

// PauseLeadResult is returned by PauseLead.
type PauseLeadResult struct {
	Email           string `json:"email"`
	PausedCampaigns int64  `json:"paused_campaigns"`
	CancelledSends  int64  `json:"cancelled_sends"`
}

// PauseLead pauses a lead across all campaigns and cancels pending sends.
func PauseLead(db *sql.DB, email string) (*PauseLeadResult, error) {
	var leadID int64
	err := db.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("lead %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up lead: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		"UPDATE campaign_leads SET status = 'paused' WHERE lead_id = ? AND status IN ('active', 'pending')",
		leadID,
	)
	if err != nil {
		return nil, fmt.Errorf("pausing campaign_leads: %w", err)
	}
	pausedCampaigns, _ := res.RowsAffected()

	res, err = tx.Exec(
		"UPDATE scheduled_sends SET status = 'cancelled' WHERE lead_id = ? AND status = 'pending'",
		leadID,
	)
	if err != nil {
		return nil, fmt.Errorf("cancelling sends: %w", err)
	}
	cancelledSends, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &PauseLeadResult{
		Email:           email,
		PausedCampaigns: pausedCampaigns,
		CancelledSends:  cancelledSends,
	}, nil
}

// BlacklistResult is returned by BlacklistLead.
type BlacklistResult struct {
	Target           string `json:"target"`
	IsDomain         bool   `json:"is_domain"`
	BlacklistedLeads int64  `json:"blacklisted_leads"`
	CancelledSends   int64  `json:"cancelled_sends"`
}

// BlacklistLead blacklists a lead by email or all leads on a domain.
func BlacklistLead(db *sql.DB, target string) (*BlacklistResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	isDomain := !strings.Contains(target, "@")
	var blacklistedLeads, cancelledSends int64

	if isDomain {
		domain := strings.ToLower(target)

		res, err := tx.Exec(
			"UPDATE leads SET global_status = 'blacklisted' WHERE domain = ? AND global_status != 'blacklisted'",
			domain,
		)
		if err != nil {
			return nil, fmt.Errorf("blacklisting domain: %w", err)
		}
		blacklistedLeads, _ = res.RowsAffected()

		res, err = tx.Exec(`
			UPDATE scheduled_sends SET status = 'cancelled'
			WHERE lead_id IN (SELECT id FROM leads WHERE domain = ?)
			AND status = 'pending'`, domain)
		if err != nil {
			return nil, fmt.Errorf("cancelling sends: %w", err)
		}
		cancelledSends, _ = res.RowsAffected()

		tx.Exec(`
			UPDATE campaign_leads SET status = 'paused'
			WHERE lead_id IN (SELECT id FROM leads WHERE domain = ?)
			AND status IN ('active', 'pending')`, domain)
	} else {
		res, err := tx.Exec(
			"UPDATE leads SET global_status = 'blacklisted' WHERE email = ? AND global_status != 'blacklisted'",
			target,
		)
		if err != nil {
			return nil, fmt.Errorf("blacklisting lead: %w", err)
		}
		blacklistedLeads, _ = res.RowsAffected()
		if blacklistedLeads == 0 {
			return nil, fmt.Errorf("lead %s not found or already blacklisted", target)
		}

		res, err = tx.Exec(`
			UPDATE scheduled_sends SET status = 'cancelled'
			WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
			AND status = 'pending'`, target)
		if err != nil {
			return nil, fmt.Errorf("cancelling sends: %w", err)
		}
		cancelledSends, _ = res.RowsAffected()

		tx.Exec(`
			UPDATE campaign_leads SET status = 'paused'
			WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
			AND status IN ('active', 'pending')`, target)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &BlacklistResult{
		Target:           target,
		IsDomain:         isDomain,
		BlacklistedLeads: blacklistedLeads,
		CancelledSends:   cancelledSends,
	}, nil
}

// CreateCampaignOpts holds options for CreateCampaign.
type CreateCampaignOpts struct {
	Name          string
	SequenceFile  string
	LeadsFile     string
	AccountEmails []string
}

// CreateCampaignResult is returned by CreateCampaign.
type CreateCampaignResult struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Leads          int    `json:"leads"`
	ScheduledSends int    `json:"scheduled_sends"`
	Accounts       int    `json:"accounts"`
}

// CreateCampaign parses sequence+CSV, validates, computes schedule, and inserts everything.
func CreateCampaign(db *sql.DB, opts CreateCampaignOpts) (*CreateCampaignResult, error) {
	// Parse sequence YAML
	seq, err := ParseSequence(opts.SequenceFile)
	if err != nil {
		return nil, err
	}

	// Parse leads CSV
	records, _, err := ParseLeadsCSV(opts.LeadsFile)
	if err != nil {
		return nil, err
	}

	// Validate template fields
	placeholders := seq.CollectPlaceholders()
	if err := ValidateLeadFields(records, placeholders); err != nil {
		return nil, err
	}

	// Resolve account emails to IDs
	var accountIDs []int64
	for _, email := range opts.AccountEmails {
		email = strings.TrimSpace(email)
		var id int64
		err := db.QueryRow("SELECT id FROM accounts WHERE email = ? AND status = 'active'", email).Scan(&id)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("account %s not found or not active", email)
		}
		if err != nil {
			return nil, fmt.Errorf("looking up account %s: %w", email, err)
		}
		accountIDs = append(accountIDs, id)
	}

	// Load config for defaults
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	sendDays, err := ParseSendDays(cfg.SendDays)
	if err != nil {
		return nil, fmt.Errorf("parsing send_days: %w", err)
	}

	tz, err := time.LoadLocation(cfg.DefaultTimezone)
	if err != nil {
		return nil, fmt.Errorf("loading timezone %s: %w", cfg.DefaultTimezone, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert campaign
	result, err := tx.Exec(`
		INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end,
			send_days, timezone, min_gap_seconds, max_gap_seconds)
		VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?)`,
		opts.Name, opts.SequenceFile,
		cfg.SendWindowStart, cfg.SendWindowEnd,
		cfg.SendDays, cfg.DefaultTimezone,
		cfg.MinGapSeconds, cfg.MaxGapSeconds,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("campaign %q already exists", opts.Name)
		}
		return nil, fmt.Errorf("inserting campaign: %w", err)
	}
	campaignID, _ := result.LastInsertId()

	// Insert campaign_accounts
	for _, accID := range accountIDs {
		if _, err := tx.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", campaignID, accID); err != nil {
			return nil, fmt.Errorf("linking account: %w", err)
		}
	}

	// Insert leads (upsert)
	var leadsForSchedule []LeadForSchedule
	for _, rec := range records {
		email := rec.Fields["email"]
		domain := ExtractDomain(email)
		firstName := rec.Fields["first_name"]
		lastName := rec.Fields["last_name"]
		company := rec.Fields["company"]

		tx.Exec(`INSERT OR IGNORE INTO leads (email, first_name, last_name, company, domain)
			VALUES (?, ?, ?, ?, ?)`,
			email, firstName, lastName, company, domain,
		)

		var leadID int64
		if err := tx.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID); err != nil {
			return nil, fmt.Errorf("looking up lead %s: %w", email, err)
		}

		var globalStatus string
		tx.QueryRow("SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
		if globalStatus == "blacklisted" || globalStatus == "bounced" {
			continue
		}

		if _, err := tx.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (?, ?, 'active')",
			campaignID, leadID); err != nil {
			return nil, fmt.Errorf("linking lead %s: %w", email, err)
		}

		leadsForSchedule = append(leadsForSchedule, LeadForSchedule{
			ID:     leadID,
			Fields: rec.Fields,
		})
	}

	if len(leadsForSchedule) == 0 {
		return nil, fmt.Errorf("no eligible leads (all blacklisted or bounced)")
	}

	// Compute schedule
	schedRows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      campaignID,
		AccountIDs:      accountIDs,
		Leads:           leadsForSchedule,
		Sequence:        seq,
		SendWindowStart: cfg.SendWindowStart,
		SendWindowEnd:   cfg.SendWindowEnd,
		SendDays:        sendDays,
		Timezone:        tz,
		MinGapSeconds:   cfg.MinGapSeconds,
		MaxGapSeconds:   cfg.MaxGapSeconds,
		StartTime:       time.Now().In(tz),
	})
	if err != nil {
		return nil, fmt.Errorf("computing schedule: %w", err)
	}

	// Insert scheduled_sends
	for _, row := range schedRows {
		if _, err := tx.Exec(`
			INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			row.CampaignID, row.LeadID, row.AccountID,
			row.StepNumber, row.VariantIndex, row.SendAt.UTC().Format(time.RFC3339),
		); err != nil {
			return nil, fmt.Errorf("inserting scheduled_send: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &CreateCampaignResult{
		ID:             campaignID,
		Name:           opts.Name,
		Status:         "draft",
		Leads:          len(leadsForSchedule),
		ScheduledSends: len(schedRows),
		Accounts:       len(accountIDs),
	}, nil
}

// PreviewRow is a row from GetCampaignPreview.
type PreviewRow struct {
	StepNumber   int    `json:"step_number"`
	VariantIndex int    `json:"variant_index"`
	SendAt       string `json:"send_at"`
	Status       string `json:"status"`
	LeadEmail    string `json:"lead_email"`
	AccountEmail string `json:"account_email"`
}

// GetCampaignPreview returns the full scheduled send list for a campaign.
func GetCampaignPreview(db *sql.DB, name string) (campaignID int64, status string, preview []PreviewRow, err error) {
	err = db.QueryRow("SELECT id, status FROM campaigns WHERE name = ?", name).Scan(&campaignID, &status)
	if err == sql.ErrNoRows {
		return 0, "", nil, fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return 0, "", nil, fmt.Errorf("looking up campaign: %w", err)
	}

	rows, err := db.Query(`
		SELECT ss.step_number, ss.variant_index, ss.send_at, ss.status,
			l.email, a.email
		FROM scheduled_sends ss
		JOIN leads l ON ss.lead_id = l.id
		JOIN accounts a ON ss.account_id = a.id
		WHERE ss.campaign_id = ?
		ORDER BY ss.send_at, ss.step_number`, campaignID)
	if err != nil {
		return 0, "", nil, fmt.Errorf("querying schedule: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r PreviewRow
		if err := rows.Scan(&r.StepNumber, &r.VariantIndex, &r.SendAt, &r.Status, &r.LeadEmail, &r.AccountEmail); err != nil {
			return 0, "", nil, fmt.Errorf("scanning row: %w", err)
		}
		preview = append(preview, r)
	}

	return campaignID, status, preview, nil
}

// CampaignStateTransition changes a campaign's status with validation.
func CampaignStateTransition(db *sql.DB, name, action, fromStatus, toStatus string) error {
	var currentStatus string
	err := db.QueryRow("SELECT status FROM campaigns WHERE name = ?", name).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return fmt.Errorf("looking up campaign: %w", err)
	}

	if currentStatus != fromStatus {
		return fmt.Errorf("cannot %s campaign %q: current status is %q (expected %q)", action, name, currentStatus, fromStatus)
	}

	if _, err := db.Exec("UPDATE campaigns SET status = ? WHERE name = ?", toStatus, name); err != nil {
		return fmt.Errorf("updating campaign: %w", err)
	}

	return nil
}

// CampaignStatusInfo is returned by GetCampaignStatus.
type CampaignStatusInfo struct {
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Sequence    string         `json:"sequence"`
	Timezone    string         `json:"timezone"`
	SendWindow  string         `json:"send_window"`
	Leads       int            `json:"leads"`
	Accounts    int            `json:"accounts"`
	TotalSends  int            `json:"total_sends"`
	SendCounts  map[string]int `json:"send_counts"`
	CreatedAt   string         `json:"created_at"`
}

// GetCampaignStatus returns campaign details and send counts.
func GetCampaignStatus(db *sql.DB, name string) (*CampaignStatusInfo, error) {
	var c struct {
		ID          int64
		Status      string
		SeqFile     string
		Timezone    string
		WindowStart string
		WindowEnd   string
		CreatedAt   string
	}
	err := db.QueryRow(`SELECT id, status, sequence_file, timezone, send_window_start, send_window_end, created_at
		FROM campaigns WHERE name = ?`, name).
		Scan(&c.ID, &c.Status, &c.SeqFile, &c.Timezone, &c.WindowStart, &c.WindowEnd, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up campaign: %w", err)
	}

	countRows, err := db.Query(`
		SELECT status, COUNT(*) FROM scheduled_sends
		WHERE campaign_id = ? GROUP BY status`, c.ID)
	if err != nil {
		return nil, fmt.Errorf("counting sends: %w", err)
	}
	defer countRows.Close()

	counts := map[string]int{}
	total := 0
	for countRows.Next() {
		var status string
		var count int
		countRows.Scan(&status, &count)
		counts[status] = count
		total += count
	}

	var leadCount, accountCount int
	db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ?", c.ID).Scan(&leadCount)
	db.QueryRow("SELECT COUNT(*) FROM campaign_accounts WHERE campaign_id = ?", c.ID).Scan(&accountCount)

	return &CampaignStatusInfo{
		Name:       name,
		Status:     c.Status,
		Sequence:   c.SeqFile,
		Timezone:   c.Timezone,
		SendWindow: c.WindowStart + " - " + c.WindowEnd,
		Leads:      leadCount,
		Accounts:   accountCount,
		TotalSends: total,
		SendCounts: counts,
		CreatedAt:  c.CreatedAt,
	}, nil
}
