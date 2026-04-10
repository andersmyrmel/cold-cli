package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

func buildLeadFields(email, firstName, lastName, company, domain, customFields string, strict bool) (map[string]string, error) {
	fields := map[string]string{
		"email":      email,
		"first_name": firstName,
		"last_name":  lastName,
		"company":    company,
		"domain":     domain,
	}

	if customFields == "" || customFields == "{}" {
		return fields, nil
	}

	var cf map[string]string
	if err := json.Unmarshal([]byte(customFields), &cf); err != nil {
		if strict {
			return nil, fmt.Errorf("parsing custom_fields JSON: %w", err)
		}
		return fields, nil
	}

	for k, v := range cf {
		if _, exists := fields[k]; !exists {
			fields[k] = v
		}
	}

	return fields, nil
}

func loadLeadFieldsStrict(db *sql.DB, leadID int64) (map[string]string, error) {
	var email, firstName, lastName, company, domain, customFields string
	err := db.QueryRow(`SELECT email, first_name, last_name, company, domain, custom_fields
		FROM leads WHERE id = ?`, leadID).
		Scan(&email, &firstName, &lastName, &company, &domain, &customFields)
	if err != nil {
		return nil, err
	}

	return buildLeadFields(email, firstName, lastName, company, domain, customFields, true)
}

func loadCampaignLeadRecords(db *sql.DB, campaignID int64) ([]LeadRecord, error) {
	rows, err := db.Query(`
		SELECT l.email, l.first_name, l.last_name, l.company, l.domain, l.custom_fields
		FROM leads l
		JOIN campaign_leads cl ON cl.lead_id = l.id
		WHERE cl.campaign_id = ?
		ORDER BY l.email`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var leads []LeadRecord
	for rows.Next() {
		var email, firstName, lastName, company, domain, customFields string
		if err := rows.Scan(&email, &firstName, &lastName, &company, &domain, &customFields); err != nil {
			return nil, err
		}

		fields, err := buildLeadFields(email, firstName, lastName, company, domain, customFields, true)
		if err != nil {
			return nil, fmt.Errorf("loading lead %s: %w", email, err)
		}

		leads = append(leads, LeadRecord{Fields: fields})
	}

	return leads, rows.Err()
}
