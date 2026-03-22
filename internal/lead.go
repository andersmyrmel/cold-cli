package internal

import (
	"database/sql"
	"fmt"
	"strings"
)

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

// ResumeLeadResult is returned by ResumeLead.
type ResumeLeadResult struct {
	Email            string `json:"email"`
	ResumedCampaigns int64  `json:"resumed_campaigns"`
	RestoredSends    int64  `json:"restored_sends"`
}

// ResumeLead resumes a paused lead: reactivates campaign_leads and restores cancelled sends.
func ResumeLead(db *sql.DB, email string) (*ResumeLeadResult, error) {
	var leadID int64
	err := db.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("lead %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up lead: %w", err)
	}

	// Check global status - can't resume blacklisted/bounced leads
	var globalStatus string
	db.QueryRow("SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
	if globalStatus == "blacklisted" || globalStatus == "bounced" {
		return nil, fmt.Errorf("lead %s is %s — cannot resume (use a new campaign to re-add)", email, globalStatus)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Only resume campaign_leads where the campaign is still active or draft
	res, err := tx.Exec(`
		UPDATE campaign_leads SET status = 'active'
		WHERE lead_id = ? AND status = 'paused'
		AND campaign_id IN (SELECT id FROM campaigns WHERE status IN ('active', 'draft'))`,
		leadID,
	)
	if err != nil {
		return nil, fmt.Errorf("resuming campaign_leads: %w", err)
	}
	resumedCampaigns, _ := res.RowsAffected()

	// Restore cancelled sends (only for active/draft campaigns, only future sends)
	res, err = tx.Exec(`
		UPDATE scheduled_sends SET status = 'pending'
		WHERE lead_id = ? AND status = 'cancelled'
		AND campaign_id IN (SELECT id FROM campaigns WHERE status IN ('active', 'draft'))`,
		leadID,
	)
	if err != nil {
		return nil, fmt.Errorf("restoring sends: %w", err)
	}
	restoredSends, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &ResumeLeadResult{
		Email:            email,
		ResumedCampaigns: resumedCampaigns,
		RestoredSends:    restoredSends,
	}, nil
}

// RemoveLeadResult is returned by RemoveLeadFromCampaign.
type RemoveLeadResult struct {
	Email          string `json:"email"`
	Campaign       string `json:"campaign"`
	CancelledSends int64  `json:"cancelled_sends"`
}

// RemoveLeadFromCampaign removes a single lead from a specific campaign.
func RemoveLeadFromCampaign(db *sql.DB, campaignName, email string) (*RemoveLeadResult, error) {
	var campaignID int64
	err := db.QueryRow("SELECT id FROM campaigns WHERE name = ?", campaignName).Scan(&campaignID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("campaign %q not found", campaignName)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up campaign: %w", err)
	}

	var leadID int64
	err = db.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("lead %s not found", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up lead: %w", err)
	}

	// Check lead is in this campaign
	var clCount int
	err = db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadID).Scan(&clCount)
	if err != nil {
		return nil, fmt.Errorf("checking campaign membership: %w", err)
	}
	if clCount == 0 {
		return nil, fmt.Errorf("lead %s is not in campaign %q", email, campaignName)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Cancel pending sends for this lead in this campaign
	res, err := tx.Exec(
		"UPDATE scheduled_sends SET status = 'cancelled' WHERE campaign_id = ? AND lead_id = ? AND status = 'pending'",
		campaignID, leadID,
	)
	if err != nil {
		return nil, fmt.Errorf("cancelling sends: %w", err)
	}
	cancelledSends, _ := res.RowsAffected()

	// Remove the campaign_lead entry
	if _, err := tx.Exec("DELETE FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?",
		campaignID, leadID); err != nil {
		return nil, fmt.Errorf("removing campaign_lead: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing: %w", err)
	}

	return &RemoveLeadResult{
		Email:          email,
		Campaign:       campaignName,
		CancelledSends: cancelledSends,
	}, nil
}

// LeadListRow is a row from ListLeads.
type LeadListRow struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	FirstName    string `json:"first_name"`
	Company      string `json:"company"`
	Domain       string `json:"domain"`
	GlobalStatus string `json:"global_status"`
	Campaigns    int    `json:"campaigns"`
}

// ListLeads returns leads, optionally filtered by domain or status.
func ListLeads(db *sql.DB, domain, status string, limit int) ([]LeadListRow, error) {
	query := `
		SELECT l.id, l.email, l.first_name, l.company, l.domain, l.global_status,
			(SELECT COUNT(*) FROM campaign_leads WHERE lead_id = l.id) as campaigns
		FROM leads l
		WHERE 1=1`

	var args []any
	if domain != "" {
		query += " AND l.domain = ?"
		args = append(args, strings.ToLower(domain))
	}
	if status != "" {
		query += " AND l.global_status = ?"
		args = append(args, status)
	}
	query += " ORDER BY l.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying leads: %w", err)
	}
	defer rows.Close()

	var leads []LeadListRow
	for rows.Next() {
		var l LeadListRow
		if err := rows.Scan(&l.ID, &l.Email, &l.FirstName, &l.Company, &l.Domain, &l.GlobalStatus, &l.Campaigns); err != nil {
			return nil, fmt.Errorf("scanning lead: %w", err)
		}
		leads = append(leads, l)
	}
	return leads, nil
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
