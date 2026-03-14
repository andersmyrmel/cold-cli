package internal

import (
	"database/sql"
	"fmt"
	"strings"
)

// ProcessReplies checks inbox messages for replies to our sent emails.
// Returns the number of replies detected.
func ProcessReplies(db *sql.DB, gws GWSClient, accounts []Account) (int, error) {
	repliesFound := 0

	for _, account := range accounts {
		// Query for recent messages in this account's inbox
		// TODO: use last_poll_at for efficient 'after:' filtering
		messages, err := gws.ListMessages(account.Email, "in:inbox")
		if err != nil {
			return repliesFound, fmt.Errorf("listing messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			if msg.InReplyTo == "" {
				continue
			}

			// Check if this is a reply to one of our sent emails
			var eventID int64
			var campaignID, leadID int64
			err := db.QueryRow(`
				SELECT e.id, e.campaign_id, e.lead_id
				FROM events e
				WHERE e.message_id = ? AND e.type = 'sent'
				LIMIT 1`,
				msg.InReplyTo,
			).Scan(&eventID, &campaignID, &leadID)

			if err == sql.ErrNoRows {
				continue // Not a reply to our email
			}
			if err != nil {
				return repliesFound, fmt.Errorf("looking up event for In-Reply-To %s: %w", msg.InReplyTo, err)
			}

			// Record the reply event
			db.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
				VALUES (?, ?, ?, 'reply', 0, ?, ?)`,
				campaignID, leadID, account.ID, msg.ID, msg.ThreadID)

			// Update lead status
			db.Exec("UPDATE campaign_leads SET status = 'replied' WHERE campaign_id = ? AND lead_id = ? AND status = 'active'",
				campaignID, leadID)

			// Skip remaining scheduled sends
			db.Exec("UPDATE scheduled_sends SET status = 'skipped' WHERE campaign_id = ? AND lead_id = ? AND status = 'pending'",
				campaignID, leadID)

			// Check stop_on_domain_reply
			var stopOnDomainReply bool
			db.QueryRow("SELECT stop_on_domain_reply FROM campaigns WHERE id = ?", campaignID).Scan(&stopOnDomainReply)

			if stopOnDomainReply {
				// Find the domain of this lead
				var domain string
				db.QueryRow("SELECT domain FROM leads WHERE id = ?", leadID).Scan(&domain)

				if domain != "" {
					// Skip all leads on the same domain in this campaign
					db.Exec(`
						UPDATE scheduled_sends SET status = 'skipped'
						WHERE campaign_id = ?
						AND lead_id IN (SELECT id FROM leads WHERE domain = ? AND id != ?)
						AND status = 'pending'`,
						campaignID, domain, leadID)

					db.Exec(`
						UPDATE campaign_leads SET status = 'paused'
						WHERE campaign_id = ?
						AND lead_id IN (SELECT id FROM leads WHERE domain = ? AND id != ?)
						AND status = 'active'`,
						campaignID, domain, leadID)
				}
			}

			repliesFound++
		}
	}

	return repliesFound, nil
}

// ProcessBounces checks inbox messages for bounce NDRs.
// Returns the number of bounces detected.
func ProcessBounces(db *sql.DB, gws GWSClient, accounts []Account) (int, error) {
	bouncesFound := 0

	for _, account := range accounts {
		messages, err := gws.ListMessages(account.Email, "from:mailer-daemon OR from:postmaster")
		if err != nil {
			return bouncesFound, fmt.Errorf("listing bounce messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			// Extract bounced email from the NDR
			bouncedEmail := extractBouncedEmail(msg.Snippet, msg.Subject)
			if bouncedEmail == "" {
				continue
			}

			// Find the lead
			var leadID int64
			err := db.QueryRow("SELECT id FROM leads WHERE email = ?", bouncedEmail).Scan(&leadID)
			if err == sql.ErrNoRows {
				continue // Not one of our leads
			}
			if err != nil {
				return bouncesFound, fmt.Errorf("looking up lead %s: %w", bouncedEmail, err)
			}

			// Mark lead as globally bounced
			db.Exec("UPDATE leads SET global_status = 'bounced' WHERE id = ? AND global_status != 'bounced'", leadID)

			// Update all campaign_leads
			db.Exec("UPDATE campaign_leads SET status = 'bounced' WHERE lead_id = ? AND status IN ('active', 'pending')", leadID)

			// Skip all pending sends across all campaigns
			db.Exec("UPDATE scheduled_sends SET status = 'skipped' WHERE lead_id = ? AND status = 'pending'", leadID)

			bouncesFound++
		}
	}

	return bouncesFound, nil
}

// extractBouncedEmail attempts to extract the bounced email address from an NDR.
// This is a basic implementation that looks for email patterns in the snippet/subject.
func extractBouncedEmail(snippet, subject string) string {
	// Common NDR patterns contain the failed address
	combined := snippet + " " + subject

	// Look for email-like patterns
	// NDRs typically say "delivery to <email> failed" or "could not be delivered to <email>"
	for _, text := range []string{combined} {
		words := strings.Fields(text)
		for _, word := range words {
			// Clean up angle brackets and common punctuation
			word = strings.Trim(word, "<>()[],.;:")
			if isLikelyEmail(word) {
				return strings.ToLower(word)
			}
		}
	}

	return ""
}

func isLikelyEmail(s string) bool {
	// Basic check: contains @, has a dot after @, no spaces
	if strings.Contains(s, " ") {
		return false
	}
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	// Must have a dot in domain
	if !strings.Contains(parts[1], ".") {
		return false
	}
	// Filter out common false positives
	domain := strings.ToLower(parts[1])
	if domain == "gmail.com" || domain == "google.com" {
		// These are typically the MAILER-DAEMON address, not the bounced address
		// But we should still check — the snippet may contain the real bounced address
	}
	return true
}
