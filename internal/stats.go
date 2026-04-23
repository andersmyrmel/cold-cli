package internal

import (
	"database/sql"
	"fmt"
)

// CampaignStats is a row from GetAllCampaignStats.
type CampaignStats struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Sent         int    `json:"sent"`
	Replies      int    `json:"replies"`
	Unsubscribes int    `json:"unsubscribes"`
	Bounces      int    `json:"bounces"`
}

// GetAllCampaignStats returns sent/replied/bounced counts per campaign.
func GetAllCampaignStats(db *sql.DB) ([]CampaignStats, error) {
	rows, err := queryDB(db, `
		SELECT c.name, c.status,
			COALESCE(SUM(CASE WHEN e.type = 'sent' THEN 1 ELSE 0 END), 0) as sent,
			COALESCE(SUM(CASE WHEN e.type = 'reply' THEN 1 ELSE 0 END), 0) as replies,
			COALESCE(SUM(CASE WHEN e.type = 'unsubscribe' THEN 1 ELSE 0 END), 0) as unsubscribes,
			COALESCE(SUM(CASE WHEN e.type = 'bounce' THEN 1 ELSE 0 END), 0) as bounces
		FROM campaigns c
		LEFT JOIN events e ON c.id = e.campaign_id
		GROUP BY c.id, c.name, c.status
		ORDER BY c.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("querying stats: %w", err)
	}
	defer rows.Close()

	var stats []CampaignStats
	for rows.Next() {
		var s CampaignStats
		rows.Scan(&s.Name, &s.Status, &s.Sent, &s.Replies, &s.Unsubscribes, &s.Bounces)
		stats = append(stats, s)
	}
	return stats, nil
}

// StepStats is a row from GetCampaignStepStats.
type StepStats struct {
	Step         int `json:"step"`
	Sent         int `json:"sent"`
	Replies      int `json:"replies"`
	Unsubscribes int `json:"unsubscribes"`
	Bounces      int `json:"bounces"`
}

// GetCampaignStepStats returns per-step stats for a campaign.
func GetCampaignStepStats(db *sql.DB, campaignID int64) ([]StepStats, error) {
	rows, err := queryDB(db, `
		SELECT e.step_number,
			SUM(CASE WHEN e.type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN e.type = 'reply' THEN 1 ELSE 0 END) as replies,
			SUM(CASE WHEN e.type = 'unsubscribe' THEN 1 ELSE 0 END) as unsubscribes,
			SUM(CASE WHEN e.type = 'bounce' THEN 1 ELSE 0 END) as bounces
		FROM events e
		WHERE e.campaign_id = ?
		GROUP BY e.step_number
		ORDER BY e.step_number`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("querying step stats: %w", err)
	}
	defer rows.Close()

	var stats []StepStats
	for rows.Next() {
		var s StepStats
		rows.Scan(&s.Step, &s.Sent, &s.Replies, &s.Unsubscribes, &s.Bounces)
		stats = append(stats, s)
	}
	return stats, nil
}

// VariantStats is a row from GetCampaignVariantStats.
type VariantStats struct {
	Step         int     `json:"step"`
	Variant      int     `json:"variant"`
	Sent         int     `json:"sent"`
	Replies      int     `json:"replies"`
	ReplyRate    float64 `json:"reply_rate"`
	Unsubscribes int     `json:"unsubscribes"`
	Bounces      int     `json:"bounces"`
}

// GetCampaignVariantStats returns per-step, per-variant stats for a campaign.
func GetCampaignVariantStats(db *sql.DB, campaignID int64) ([]VariantStats, error) {
	rows, err := queryDB(db, `
		SELECT ss.step_number, ss.variant_index,
			COUNT(DISTINCT CASE WHEN ss.status = 'sent' THEN ss.id END) as sent,
			COUNT(DISTINCT CASE WHEN e.type = 'reply' THEN e.id END) as replies,
			COUNT(DISTINCT CASE WHEN e.type = 'unsubscribe' THEN e.id END) as unsubscribes,
			COUNT(DISTINCT CASE WHEN e.type = 'bounce' THEN e.id END) as bounces
		FROM scheduled_sends ss
		LEFT JOIN events e ON e.campaign_id = ss.campaign_id
			AND e.lead_id = ss.lead_id
			AND e.type IN ('reply', 'unsubscribe', 'bounce')
		WHERE ss.campaign_id = ?
		GROUP BY ss.step_number, ss.variant_index
		ORDER BY ss.step_number, ss.variant_index`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("querying variant stats: %w", err)
	}
	defer rows.Close()

	var stats []VariantStats
	for rows.Next() {
		var s VariantStats
		rows.Scan(&s.Step, &s.Variant, &s.Sent, &s.Replies, &s.Unsubscribes, &s.Bounces)
		if s.Sent > 0 {
			s.ReplyRate = float64(s.Replies) / float64(s.Sent) * 100
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// LeadStatsRow is a row from GetCampaignLeadStats.
type LeadStatsRow struct {
	Email     string  `json:"email"`
	Status    string  `json:"status"`
	StepsSent int     `json:"steps_sent"`
	ReplyAt   *string `json:"reply_at,omitempty"`
}

// EventLogRow is a row from GetEventLog.
type EventLogRow struct {
	Timestamp    string `json:"timestamp"`
	Type         string `json:"type"`
	Campaign     string `json:"campaign"`
	LeadEmail    string `json:"lead_email"`
	AccountEmail string `json:"account_email"`
	StepNumber   int    `json:"step_number"`
	MessageID    string `json:"message_id,omitempty"`
}

// GetEventLog returns the most recent events, optionally filtered by campaign.
func GetEventLog(db *sql.DB, campaignName string, limit int) ([]EventLogRow, error) {
	query := `
		SELECT e.timestamp, e.type, c.name, l.email, a.email, e.step_number, e.message_id
		FROM events e
		JOIN campaigns c ON e.campaign_id = c.id
		JOIN leads l ON e.lead_id = l.id
		JOIN accounts a ON e.account_id = a.id`

	var args []any
	if campaignName != "" {
		query += " WHERE c.name = ?"
		args = append(args, campaignName)
	}
	query += " ORDER BY e.timestamp DESC, e.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying event log: %w", err)
	}
	defer rows.Close()

	var events []EventLogRow
	for rows.Next() {
		var e EventLogRow
		if err := rows.Scan(&e.Timestamp, &e.Type, &e.Campaign, &e.LeadEmail, &e.AccountEmail, &e.StepNumber, &e.MessageID); err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		events = append(events, e)
	}
	return events, nil
}

// GetCampaignLeadStats returns per-lead stats for a campaign.
func GetCampaignLeadStats(db *sql.DB, campaignID int64) ([]LeadStatsRow, error) {
	rows, err := queryDB(db, `
		SELECT l.email, cl.status,
			(SELECT COUNT(*) FROM events e WHERE e.lead_id = l.id AND e.campaign_id = ? AND e.type = 'sent') as steps_sent,
			(SELECT MAX(e.timestamp) FROM events e WHERE e.lead_id = l.id AND e.campaign_id = ? AND e.type = 'reply') as reply_at
		FROM campaign_leads cl
		JOIN leads l ON cl.lead_id = l.id
		WHERE cl.campaign_id = ?
		ORDER BY l.email`, campaignID, campaignID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("querying lead stats: %w", err)
	}
	defer rows.Close()

	var stats []LeadStatsRow
	for rows.Next() {
		var s LeadStatsRow
		var replyAt sql.NullString
		rows.Scan(&s.Email, &s.Status, &s.StepsSent, &replyAt)
		if replyAt.Valid {
			s.ReplyAt = &replyAt.String
		}
		stats = append(stats, s)
	}
	return stats, nil
}
