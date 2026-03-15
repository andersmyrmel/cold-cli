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
	rows, err := db.Query(`
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
	rows, err := db.Query(`
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

// LeadStatsRow is a row from GetCampaignLeadStats.
type LeadStatsRow struct {
	Email     string  `json:"email"`
	Status    string  `json:"status"`
	StepsSent int     `json:"steps_sent"`
	ReplyAt   *string `json:"reply_at,omitempty"`
}

// GetCampaignLeadStats returns per-lead stats for a campaign.
func GetCampaignLeadStats(db *sql.DB, campaignID int64) ([]LeadStatsRow, error) {
	rows, err := db.Query(`
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
