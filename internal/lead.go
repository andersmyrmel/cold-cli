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
