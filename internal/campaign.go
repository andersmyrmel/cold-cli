package internal

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ResolveCampaignName accepts a campaign name or numeric ID and returns the campaign name.
// This lets users reference campaigns by either their name or the ID shown in "campaign list".
func ResolveCampaignName(db *sql.DB, nameOrID string) (string, error) {
	// Try as numeric ID first
	if id, err := strconv.ParseInt(nameOrID, 10, 64); err == nil {
		var name string
		err := db.QueryRow("SELECT name FROM campaigns WHERE id = ?", id).Scan(&name)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("campaign with ID %d not found", id)
		}
		if err != nil {
			return "", fmt.Errorf("looking up campaign by ID: %w", err)
		}
		return name, nil
	}
	// Otherwise treat as name — verify it exists
	var name string
	err := db.QueryRow("SELECT name FROM campaigns WHERE name = ?", nameOrID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("campaign %q not found", nameOrID)
	}
	if err != nil {
		return "", fmt.Errorf("looking up campaign: %w", err)
	}
	return name, nil
}

// CreateCampaignOpts holds options for CreateCampaign.
type CreateCampaignOpts struct {
	Name          string
	SequenceFile  string
	LeadsFile     string
	AccountEmails []string
	StartDate     string // optional "YYYY-MM-DD"; empty = now
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
	seq, err := ParseSequence(opts.SequenceFile)
	if err != nil {
		return nil, err
	}

	seqContent, err := os.ReadFile(opts.SequenceFile)
	if err != nil {
		return nil, fmt.Errorf("reading sequence file: %w", err)
	}

	records, _, err := ParseLeadsCSV(opts.LeadsFile)
	if err != nil {
		return nil, err
	}

	placeholders := seq.CollectPlaceholders()
	if err := ValidateLeadFields(records, placeholders); err != nil {
		return nil, err
	}

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

	result, err := tx.Exec(`
		INSERT INTO campaigns (name, status, sequence_file, sequence_content, send_window_start, send_window_end,
			send_days, timezone, min_gap_seconds, max_gap_seconds)
		VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?)`,
		opts.Name, opts.SequenceFile, string(seqContent),
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

	for _, accID := range accountIDs {
		if _, err := tx.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", campaignID, accID); err != nil {
			return nil, fmt.Errorf("linking account: %w", err)
		}
	}

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
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, fmt.Errorf("lead %s is already in this campaign", email)
			}
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

	startTime := time.Now().In(tz)
	if opts.StartDate != "" {
		parsed, err := time.ParseInLocation("2006-01-02", opts.StartDate, tz)
		if err != nil {
			return nil, fmt.Errorf("invalid start date %q (expected YYYY-MM-DD): %w", opts.StartDate, err)
		}
		// Use window start on the given date
		ws, _ := parseTimeOfDay(cfg.SendWindowStart)
		startTime = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), ws.hour, ws.min, 0, 0, tz)
	}

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
		StartTime:       startTime,
	})
	if err != nil {
		return nil, fmt.Errorf("computing schedule: %w", err)
	}

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

// DailyLimitWarning describes a day where scheduled sends exceed an account's daily limit.
type DailyLimitWarning struct {
	Date      string `json:"date"`
	Account   string `json:"account"`
	Scheduled int    `json:"scheduled"`
	Limit     int    `json:"limit"`
	Overflow  int    `json:"overflow"`
}

// GetDailyLimitWarnings checks all pending sends across all active campaigns for each account
// and returns warnings for days that exceed the account's daily limit.
func GetDailyLimitWarnings(db *sql.DB) ([]DailyLimitWarning, error) {
	rows, err := db.Query(`
		SELECT DATE(ss.send_at) as send_date, a.email, COUNT(*) as cnt, a.daily_limit
		FROM scheduled_sends ss
		JOIN accounts a ON ss.account_id = a.id
		JOIN campaigns c ON ss.campaign_id = c.id
		WHERE ss.status = 'pending'
		  AND c.status IN ('active', 'draft')
		GROUP BY DATE(ss.send_at), a.id
		HAVING cnt > a.daily_limit
		ORDER BY send_date, a.email`)
	if err != nil {
		return nil, fmt.Errorf("querying daily limits: %w", err)
	}
	defer rows.Close()

	var warnings []DailyLimitWarning
	for rows.Next() {
		var w DailyLimitWarning
		if err := rows.Scan(&w.Date, &w.Account, &w.Scheduled, &w.Limit); err != nil {
			return nil, fmt.Errorf("scanning warning: %w", err)
		}
		w.Overflow = w.Scheduled - w.Limit
		warnings = append(warnings, w)
	}
	return warnings, nil
}

// RenderedEmail is a preview of an actual email with templates filled in.
type RenderedEmail struct {
	StepNumber   int    `json:"step_number"`
	VariantIndex int    `json:"variant_index"`
	LeadEmail    string `json:"lead_email"`
	AccountEmail string `json:"account_email"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
}

// GetCampaignRenderedPreview returns rendered emails for a specific lead (or the first lead) in a campaign.
func GetCampaignRenderedPreview(db *sql.DB, name string, leadEmail string) ([]RenderedEmail, error) {
	var campaignID int64
	var seqContent string
	err := db.QueryRow("SELECT id, sequence_content FROM campaigns WHERE name = ?", name).
		Scan(&campaignID, &seqContent)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up campaign: %w", err)
	}

	if seqContent == "" {
		return nil, fmt.Errorf("campaign has no stored sequence content")
	}

	seq, err := ParseSequenceFromBytes([]byte(seqContent))
	if err != nil {
		return nil, fmt.Errorf("parsing sequence: %w", err)
	}

	// Get the target lead's scheduled sends (specific lead or first lead)
	var leadQuery string
	var leadArgs []any
	if leadEmail != "" {
		leadQuery = `
			SELECT ss.step_number, ss.variant_index, l.email, a.email, l.id
			FROM scheduled_sends ss
			JOIN leads l ON ss.lead_id = l.id
			JOIN accounts a ON ss.account_id = a.id
			WHERE ss.campaign_id = ? AND l.email = ?
			ORDER BY ss.send_at, ss.step_number
			LIMIT 1`
		leadArgs = []any{campaignID, leadEmail}
	} else {
		leadQuery = `
			SELECT ss.step_number, ss.variant_index, l.email, a.email, l.id
			FROM scheduled_sends ss
			JOIN leads l ON ss.lead_id = l.id
			JOIN accounts a ON ss.account_id = a.id
			WHERE ss.campaign_id = ?
			ORDER BY ss.send_at, ss.step_number
			LIMIT 1`
		leadArgs = []any{campaignID}
	}
	rows, err := db.Query(leadQuery, leadArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying first lead: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if leadEmail != "" {
			return nil, fmt.Errorf("lead %s not found in campaign %q", leadEmail, name)
		}
		return nil, fmt.Errorf("campaign has no scheduled sends")
	}

	var firstLeadID int64
	var firstLeadEmail, firstAccountEmail string
	var stepNum, varIdx int
	if err := rows.Scan(&stepNum, &varIdx, &firstLeadEmail, &firstAccountEmail, &firstLeadID); err != nil {
		return nil, fmt.Errorf("scanning first lead: %w", err)
	}
	rows.Close()

	// Load lead fields (custom_fields JSON + standard fields)
	var firstName, lastName, company, domain, customJSON string
	db.QueryRow("SELECT first_name, last_name, company, domain, custom_fields FROM leads WHERE id = ?",
		firstLeadID).Scan(&firstName, &lastName, &company, &domain, &customJSON)

	fields := map[string]string{
		"email":      firstLeadEmail,
		"first_name": firstName,
		"last_name":  lastName,
		"company":    company,
		"domain":     domain,
	}

	// Get all sends for this lead in this campaign
	sendRows, err := db.Query(`
		SELECT ss.step_number, ss.variant_index, a.email
		FROM scheduled_sends ss
		JOIN accounts a ON ss.account_id = a.id
		WHERE ss.campaign_id = ? AND ss.lead_id = ?
		ORDER BY ss.step_number`, campaignID, firstLeadID)
	if err != nil {
		return nil, fmt.Errorf("querying lead sends: %w", err)
	}
	defer sendRows.Close()

	var rendered []RenderedEmail
	for sendRows.Next() {
		var sn, vi int
		var accEmail string
		sendRows.Scan(&sn, &vi, &accEmail)

		params := BuildEmailForSend(seq, sn, vi, fields, accEmail)
		rendered = append(rendered, RenderedEmail{
			StepNumber:   sn,
			VariantIndex: vi,
			LeadEmail:    firstLeadEmail,
			AccountEmail: accEmail,
			Subject:      params.Subject,
			Body:         params.Body,
		})
	}

	return rendered, nil
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
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Sequence   string         `json:"sequence"`
	Timezone   string         `json:"timezone"`
	SendWindow string         `json:"send_window"`
	SendDays   string         `json:"send_days"`
	Leads      int            `json:"leads"`
	Accounts   int            `json:"accounts"`
	TotalSends int            `json:"total_sends"`
	SendCounts map[string]int `json:"send_counts"`
	CreatedAt  string         `json:"created_at"`
	ReplyRate  *float64       `json:"reply_rate,omitempty"`
	NextSendAt *string        `json:"next_send_at,omitempty"`
	LastSendAt *string        `json:"last_send_at,omitempty"`
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
		SendDays    string
		CreatedAt   string
	}
	err := db.QueryRow(`SELECT id, status, sequence_file, timezone, send_window_start, send_window_end, send_days, created_at
		FROM campaigns WHERE name = ?`, name).
		Scan(&c.ID, &c.Status, &c.SeqFile, &c.Timezone, &c.WindowStart, &c.WindowEnd, &c.SendDays, &c.CreatedAt)
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

	info := &CampaignStatusInfo{
		Name:       name,
		Status:     c.Status,
		Sequence:   c.SeqFile,
		Timezone:   c.Timezone,
		SendWindow: c.WindowStart + " - " + c.WindowEnd,
		SendDays:   FormatSendDays(c.SendDays),
		Leads:      leadCount,
		Accounts:   accountCount,
		TotalSends: total,
		SendCounts: counts,
		CreatedAt:  c.CreatedAt,
	}

	// Reply rate: replies / sent
	sent := counts["sent"]
	var replyCount int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE campaign_id = ? AND type = 'reply'", c.ID).Scan(&replyCount)
	if sent > 0 {
		rate := float64(replyCount) / float64(sent) * 100
		info.ReplyRate = &rate
	}

	// Next pending send
	var nextSend sql.NullString
	db.QueryRow("SELECT MIN(send_at) FROM scheduled_sends WHERE campaign_id = ? AND status = 'pending'",
		c.ID).Scan(&nextSend)
	if nextSend.Valid {
		info.NextSendAt = &nextSend.String
	}

	// Last sent
	var lastSend sql.NullString
	db.QueryRow("SELECT MAX(sent_at) FROM scheduled_sends WHERE campaign_id = ? AND status = 'sent'",
		c.ID).Scan(&lastSend)
	if lastSend.Valid {
		info.LastSendAt = &lastSend.String
	}

	return info, nil
}

// CampaignListRow is a row from ListCampaigns.
type CampaignListRow struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Leads      int    `json:"leads"`
	Sends      int    `json:"sends"`
	SendWindow string `json:"send_window"`
	SendDays   string `json:"send_days"`
}

// ListCampaigns returns all campaigns with lead and send counts.
func ListCampaigns(db *sql.DB) ([]CampaignListRow, error) {
	rows, err := db.Query(`
		SELECT c.id, c.name, c.status,
			(SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = c.id) as leads,
			(SELECT COUNT(*) FROM scheduled_sends WHERE campaign_id = c.id) as sends,
			c.send_window_start, c.send_window_end, c.send_days
		FROM campaigns c
		ORDER BY c.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing campaigns: %w", err)
	}
	defer rows.Close()

	var campaigns []CampaignListRow
	for rows.Next() {
		var c CampaignListRow
		var windowStart, windowEnd, sendDays string
		rows.Scan(&c.ID, &c.Name, &c.Status, &c.Leads, &c.Sends, &windowStart, &windowEnd, &sendDays)
		c.SendWindow = windowStart + "-" + windowEnd
		c.SendDays = FormatSendDays(sendDays)
		campaigns = append(campaigns, c)
	}
	return campaigns, nil
}

// FormatSendDays converts "1,2,3,4,5" to "Mon-Fri" or similar human-readable format.
func FormatSendDays(s string) string {
	dayNames := map[string]string{
		"0": "Sun", "1": "Mon", "2": "Tue", "3": "Wed",
		"4": "Thu", "5": "Fri", "6": "Sat",
	}
	parts := strings.Split(s, ",")
	var names []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if name, ok := dayNames[p]; ok {
			names = append(names, name)
		}
	}

	// Detect common ranges
	if len(names) > 2 {
		days, _ := ParseSendDays(s)
		if isContiguousRange(days) {
			return names[0] + "-" + names[len(names)-1]
		}
	}
	return strings.Join(names, ",")
}

func isContiguousRange(days []time.Weekday) bool {
	for i := 1; i < len(days); i++ {
		if days[i] != days[i-1]+1 {
			return false
		}
	}
	return true
}

// DeleteCampaign deletes a campaign and all associated data.
func DeleteCampaign(db *sql.DB, name string) (int64, error) {
	var campaignID int64
	err := db.QueryRow("SELECT id FROM campaigns WHERE name = ?", name).Scan(&campaignID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return 0, fmt.Errorf("looking up campaign: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	tx.Exec("DELETE FROM scheduled_sends WHERE campaign_id = ?", campaignID)
	tx.Exec("DELETE FROM events WHERE campaign_id = ?", campaignID)
	tx.Exec("DELETE FROM campaign_leads WHERE campaign_id = ?", campaignID)
	tx.Exec("DELETE FROM campaign_accounts WHERE campaign_id = ?", campaignID)
	tx.Exec("DELETE FROM campaigns WHERE id = ?", campaignID)

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing: %w", err)
	}

	return campaignID, nil
}

// CloneCampaignOpts holds options for CloneCampaign.
type CloneCampaignOpts struct {
	SourceName string
	NewName    string
	LeadsFile  string
	Accounts   []string // optional: override accounts; empty = reuse source accounts
}

// CloneCampaign creates a new campaign by copying settings from an existing one with new leads.
func CloneCampaign(db *sql.DB, opts CloneCampaignOpts) (*CreateCampaignResult, error) {
	// Load source campaign
	var src struct {
		ID              int64
		SeqFile         string
		SeqContent      string
		StopOnReply     bool
		StopOnDomain    bool
		WindowStart     string
		WindowEnd       string
		SendDays        string
		Timezone        string
		MinGap          int
		MaxGap          int
	}
	err := db.QueryRow(`SELECT id, sequence_file, sequence_content, stop_on_reply, stop_on_domain_reply,
		send_window_start, send_window_end, send_days, timezone, min_gap_seconds, max_gap_seconds
		FROM campaigns WHERE name = ?`, opts.SourceName).
		Scan(&src.ID, &src.SeqFile, &src.SeqContent, &src.StopOnReply, &src.StopOnDomain,
			&src.WindowStart, &src.WindowEnd, &src.SendDays, &src.Timezone, &src.MinGap, &src.MaxGap)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("source campaign %q not found", opts.SourceName)
	}
	if err != nil {
		return nil, fmt.Errorf("loading source campaign: %w", err)
	}

	// Parse sequence from stored content or file
	var seq *Sequence
	if src.SeqContent != "" {
		seq, err = ParseSequenceFromBytes([]byte(src.SeqContent))
	} else {
		seq, err = ParseSequence(src.SeqFile)
	}
	if err != nil {
		return nil, fmt.Errorf("parsing sequence: %w", err)
	}

	// Parse leads
	records, _, err := ParseLeadsCSV(opts.LeadsFile)
	if err != nil {
		return nil, err
	}

	placeholders := seq.CollectPlaceholders()
	if err := ValidateLeadFields(records, placeholders); err != nil {
		return nil, err
	}

	// Resolve accounts
	var accountIDs []int64
	if len(opts.Accounts) > 0 {
		for _, email := range opts.Accounts {
			email = strings.TrimSpace(email)
			var id int64
			if err := db.QueryRow("SELECT id FROM accounts WHERE email = ? AND status = 'active'", email).Scan(&id); err != nil {
				return nil, fmt.Errorf("account %s not found or not active", email)
			}
			accountIDs = append(accountIDs, id)
		}
	} else {
		// Reuse source campaign's accounts
		rows, err := db.Query("SELECT account_id FROM campaign_accounts WHERE campaign_id = ?", src.ID)
		if err != nil {
			return nil, fmt.Errorf("loading source accounts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			accountIDs = append(accountIDs, id)
		}
	}
	if len(accountIDs) == 0 {
		return nil, fmt.Errorf("no accounts available")
	}

	sendDays, err := ParseSendDays(src.SendDays)
	if err != nil {
		return nil, fmt.Errorf("parsing send_days: %w", err)
	}
	tz, err := time.LoadLocation(src.Timezone)
	if err != nil {
		return nil, fmt.Errorf("loading timezone: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Create new campaign with same settings
	stopReply := 0
	if src.StopOnReply {
		stopReply = 1
	}
	stopDomain := 0
	if src.StopOnDomain {
		stopDomain = 1
	}

	res, err := tx.Exec(`
		INSERT INTO campaigns (name, status, sequence_file, sequence_content, stop_on_reply, stop_on_domain_reply,
			send_window_start, send_window_end, send_days, timezone, min_gap_seconds, max_gap_seconds)
		VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		opts.NewName, src.SeqFile, src.SeqContent, stopReply, stopDomain,
		src.WindowStart, src.WindowEnd, src.SendDays, src.Timezone, src.MinGap, src.MaxGap)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("campaign %q already exists", opts.NewName)
		}
		return nil, fmt.Errorf("inserting campaign: %w", err)
	}
	campaignID, _ := res.LastInsertId()

	for _, accID := range accountIDs {
		tx.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", campaignID, accID)
	}

	// Insert leads and compute schedule
	leadsAdded, sendsAdded, err := insertLeadsAndSchedule(tx, campaignID, accountIDs, records, seq,
		src.WindowStart, src.WindowEnd, sendDays, tz, src.MinGap, src.MaxGap)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &CreateCampaignResult{
		ID:             campaignID,
		Name:           opts.NewName,
		Status:         "draft",
		Leads:          leadsAdded,
		ScheduledSends: sendsAdded,
		Accounts:       len(accountIDs),
	}, nil
}

// AddLeadsResult is returned by AddLeadsToCampaign.
type AddLeadsResult struct {
	Campaign       string `json:"campaign"`
	LeadsAdded     int    `json:"leads_added"`
	LeadsSkipped   int    `json:"leads_skipped"`
	ScheduledSends int    `json:"scheduled_sends"`
}

// AddLeadsToCampaign adds new leads to an existing campaign and schedules their sends.
func AddLeadsToCampaign(db *sql.DB, campaignName, leadsFile string) (*AddLeadsResult, error) {
	// Load campaign
	var campID int64
	var seqFile, seqContent, windowStart, windowEnd, sendDaysStr, tzName string
	var minGap, maxGap int
	err := db.QueryRow(`SELECT id, sequence_file, sequence_content, send_window_start, send_window_end,
		send_days, timezone, min_gap_seconds, max_gap_seconds
		FROM campaigns WHERE name = ?`, campaignName).
		Scan(&campID, &seqFile, &seqContent, &windowStart, &windowEnd, &sendDaysStr, &tzName, &minGap, &maxGap)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign %q not found", campaignName)
	}
	if err != nil {
		return nil, fmt.Errorf("loading campaign: %w", err)
	}

	// Parse sequence
	var seq *Sequence
	if seqContent != "" {
		seq, err = ParseSequenceFromBytes([]byte(seqContent))
	} else {
		seq, err = ParseSequence(seqFile)
	}
	if err != nil {
		return nil, fmt.Errorf("parsing sequence: %w", err)
	}

	// Parse leads
	records, _, err := ParseLeadsCSV(leadsFile)
	if err != nil {
		return nil, err
	}

	placeholders := seq.CollectPlaceholders()
	if err := ValidateLeadFields(records, placeholders); err != nil {
		return nil, err
	}

	// Get campaign accounts
	var accountIDs []int64
	rows, err := db.Query("SELECT account_id FROM campaign_accounts WHERE campaign_id = ?", campID)
	if err != nil {
		return nil, fmt.Errorf("loading accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		accountIDs = append(accountIDs, id)
	}
	if len(accountIDs) == 0 {
		return nil, fmt.Errorf("campaign has no accounts")
	}

	sendDays, err := ParseSendDays(sendDaysStr)
	if err != nil {
		return nil, fmt.Errorf("parsing send_days: %w", err)
	}
	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("loading timezone: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	totalRecords := len(records)
	leadsAdded, sendsAdded, err := insertLeadsAndSchedule(tx, campID, accountIDs, records, seq,
		windowStart, windowEnd, sendDays, tz, minGap, maxGap)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &AddLeadsResult{
		Campaign:       campaignName,
		LeadsAdded:     leadsAdded,
		LeadsSkipped:   totalRecords - leadsAdded,
		ScheduledSends: sendsAdded,
	}, nil
}

// insertLeadsAndSchedule is the shared logic for creating leads and their scheduled sends.
// It inserts/upserts leads, links them to the campaign, computes schedule, and inserts sends.
// Returns the number of leads added and sends created.
func insertLeadsAndSchedule(tx *sql.Tx, campaignID int64, accountIDs []int64,
	records []LeadRecord, seq *Sequence,
	windowStart, windowEnd string, sendDays []time.Weekday, tz *time.Location,
	minGap, maxGap int) (leadsAdded int, sendsAdded int, err error) {

	var leadsForSchedule []LeadForSchedule
	for _, rec := range records {
		email := rec.Fields["email"]
		domain := ExtractDomain(email)
		firstName := rec.Fields["first_name"]
		lastName := rec.Fields["last_name"]
		company := rec.Fields["company"]

		tx.Exec(`INSERT OR IGNORE INTO leads (email, first_name, last_name, company, domain)
			VALUES (?, ?, ?, ?, ?)`,
			email, firstName, lastName, company, domain)

		var leadID int64
		if err := tx.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID); err != nil {
			return 0, 0, fmt.Errorf("looking up lead %s: %w", email, err)
		}

		var globalStatus string
		tx.QueryRow("SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
		if globalStatus == "blacklisted" || globalStatus == "bounced" {
			continue
		}

		// Skip if already in this campaign
		var existing int
		tx.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?",
			campaignID, leadID).Scan(&existing)
		if existing > 0 {
			continue
		}

		if _, err := tx.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (?, ?, 'active')",
			campaignID, leadID); err != nil {
			return 0, 0, fmt.Errorf("linking lead %s: %w", email, err)
		}

		leadsForSchedule = append(leadsForSchedule, LeadForSchedule{
			ID:     leadID,
			Fields: rec.Fields,
		})
	}

	if len(leadsForSchedule) == 0 {
		return 0, 0, nil
	}

	schedRows, err := ComputeSchedule(ScheduleConfig{
		CampaignID:      campaignID,
		AccountIDs:      accountIDs,
		Leads:           leadsForSchedule,
		Sequence:        seq,
		SendWindowStart: windowStart,
		SendWindowEnd:   windowEnd,
		SendDays:        sendDays,
		Timezone:        tz,
		MinGapSeconds:   minGap,
		MaxGapSeconds:   maxGap,
		StartTime:       time.Now().In(tz),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("computing schedule: %w", err)
	}

	for _, row := range schedRows {
		if _, err := tx.Exec(`
			INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			row.CampaignID, row.LeadID, row.AccountID,
			row.StepNumber, row.VariantIndex, row.SendAt.UTC().Format(time.RFC3339),
		); err != nil {
			return 0, 0, fmt.Errorf("inserting scheduled_send: %w", err)
		}
	}

	return len(leadsForSchedule), len(schedRows), nil
}

// RetryCampaignResult is returned by RetryCampaign.
type RetryCampaignResult struct {
	Campaign string `json:"campaign"`
	Retried  int    `json:"retried"`
}

// RetryCampaign resets failed sends back to pending with send_at = now.
// If step is non-nil, only retry failed sends for that specific step.
func RetryCampaign(db *sql.DB, name string, step *int) (*RetryCampaignResult, error) {
	var campaignID int64
	err := db.QueryRow("SELECT id FROM campaigns WHERE name = ?", name).Scan(&campaignID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up campaign: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	var res sql.Result
	if step != nil {
		res, err = db.Exec(`UPDATE scheduled_sends SET status = 'pending', send_at = ?
			WHERE campaign_id = ? AND status = 'failed' AND step_number = ?`,
			now, campaignID, *step)
	} else {
		res, err = db.Exec(`UPDATE scheduled_sends SET status = 'pending', send_at = ?
			WHERE campaign_id = ? AND status = 'failed'`,
			now, campaignID)
	}
	if err != nil {
		return nil, fmt.Errorf("retrying sends: %w", err)
	}

	affected, _ := res.RowsAffected()
	return &RetryCampaignResult{
		Campaign: name,
		Retried:  int(affected),
	}, nil
}

// UpdateCampaignOpts holds fields to update. Zero values are ignored.
type UpdateCampaignOpts struct {
	SendWindowStart *string
	SendWindowEnd   *string
	SendDays        *string
	Timezone        *string
	MinGapSeconds   *int
	MaxGapSeconds   *int
	SequenceFile    *string // path to new sequence YAML
}

// UpdateCampaign updates campaign settings with validation.
func UpdateCampaign(db *sql.DB, name string, opts UpdateCampaignOpts) error {
	// Validate inputs before touching the database
	if opts.Timezone != nil {
		if _, err := time.LoadLocation(*opts.Timezone); err != nil {
			return fmt.Errorf("invalid timezone %q: %w", *opts.Timezone, err)
		}
	}
	if opts.SendWindowStart != nil {
		if _, err := parseTimeOfDay(*opts.SendWindowStart); err != nil {
			return fmt.Errorf("invalid send_window_start: %w", err)
		}
	}
	if opts.SendWindowEnd != nil {
		if _, err := parseTimeOfDay(*opts.SendWindowEnd); err != nil {
			return fmt.Errorf("invalid send_window_end: %w", err)
		}
	}
	if opts.SendDays != nil {
		if _, err := ParseSendDays(*opts.SendDays); err != nil {
			return fmt.Errorf("invalid send_days: %w", err)
		}
	}

	var campaignID int64
	err := db.QueryRow("SELECT id FROM campaigns WHERE name = ?", name).Scan(&campaignID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("campaign %q not found", name)
	}
	if err != nil {
		return fmt.Errorf("looking up campaign: %w", err)
	}

	// Validate and prepare sequence update if requested
	var newSeqFile string
	var newSeqContent string
	if opts.SequenceFile != nil {
		seq, err := ParseSequence(*opts.SequenceFile)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(*opts.SequenceFile)
		if err != nil {
			return fmt.Errorf("reading sequence file: %w", err)
		}

		// Validate placeholders against existing campaign leads
		placeholders := seq.CollectPlaceholders()
		if len(placeholders) > 0 {
			rows, err := db.Query(`
				SELECT l.email, l.first_name, l.last_name, l.company, l.domain
				FROM leads l
				JOIN campaign_leads cl ON cl.lead_id = l.id
				WHERE cl.campaign_id = ?`, campaignID)
			if err != nil {
				return fmt.Errorf("loading campaign leads: %w", err)
			}
			defer rows.Close()

			var leads []LeadRecord
			for rows.Next() {
				var email, firstName, lastName, company, domain string
				rows.Scan(&email, &firstName, &lastName, &company, &domain)
				leads = append(leads, LeadRecord{
					Fields: map[string]string{
						"email":      email,
						"first_name": firstName,
						"last_name":  lastName,
						"company":    company,
						"domain":     domain,
					},
				})
			}
			if err := ValidateLeadFields(leads, placeholders); err != nil {
				return fmt.Errorf("new sequence has placeholders that existing leads can't fill:\n%w", err)
			}
		}

		newSeqFile = *opts.SequenceFile
		newSeqContent = string(content)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	updates := []struct {
		col string
		val any
	}{}
	if opts.SendWindowStart != nil {
		updates = append(updates, struct{ col string; val any }{"send_window_start", *opts.SendWindowStart})
	}
	if opts.SendWindowEnd != nil {
		updates = append(updates, struct{ col string; val any }{"send_window_end", *opts.SendWindowEnd})
	}
	if opts.SendDays != nil {
		updates = append(updates, struct{ col string; val any }{"send_days", *opts.SendDays})
	}
	if opts.Timezone != nil {
		updates = append(updates, struct{ col string; val any }{"timezone", *opts.Timezone})
	}
	if opts.MinGapSeconds != nil {
		updates = append(updates, struct{ col string; val any }{"min_gap_seconds", *opts.MinGapSeconds})
	}
	if opts.MaxGapSeconds != nil {
		updates = append(updates, struct{ col string; val any }{"max_gap_seconds", *opts.MaxGapSeconds})
	}
	if opts.SequenceFile != nil {
		updates = append(updates, struct{ col string; val any }{"sequence_file", newSeqFile})
		updates = append(updates, struct{ col string; val any }{"sequence_content", newSeqContent})
	}

	for _, u := range updates {
		if _, err := tx.Exec("UPDATE campaigns SET "+u.col+" = ? WHERE id = ?", u.val, campaignID); err != nil {
			return fmt.Errorf("updating %s: %w", u.col, err)
		}
	}

	return tx.Commit()
}
