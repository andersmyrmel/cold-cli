package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// GetLastPollAt returns the last poll timestamp, or a default (24h ago).
func GetLastPollAt(db *sql.DB) time.Time {
	var val string
	err := db.QueryRow("SELECT value FROM kv WHERE key = 'last_poll_at'").Scan(&val)
	if err != nil {
		return time.Now().Add(-24 * time.Hour)
	}
	t, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return time.Now().Add(-24 * time.Hour)
	}
	return t
}

// SetLastPollAt updates the last poll timestamp.
func SetLastPollAt(db *sql.DB, t time.Time) {
	db.Exec(`INSERT INTO kv (key, value) VALUES ('last_poll_at', ?)
		ON CONFLICT(key) DO UPDATE SET value = ?`,
		t.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
}

// ProcessReplies checks inbox messages for replies to our sent emails.
// Returns the number of new replies detected.
func ProcessReplies(db *sql.DB, gws GWSClient, accounts []Account) (int, error) {
	repliesFound := 0
	lastPoll := GetLastPollAt(db)

	// Gmail 'after:' uses epoch seconds
	afterFilter := fmt.Sprintf("in:inbox after:%d", lastPoll.Unix())

	for _, account := range accounts {
		messages, err := gws.ListMessages(account.Email, afterFilter)
		if err != nil {
			return repliesFound, fmt.Errorf("listing messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			if msg.InReplyTo == "" {
				continue
			}

			// Dedup: skip if we already recorded this message as a reply event
			var existing int
			db.QueryRow("SELECT COUNT(*) FROM events WHERE message_id = ? AND type = 'reply'", msg.ID).Scan(&existing)
			if existing > 0 {
				continue
			}

			// Check if this is a reply to one of our sent emails
			var campaignID, leadID int64
			err := db.QueryRow(`
				SELECT e.campaign_id, e.lead_id
				FROM events e
				WHERE e.message_id = ? AND e.type = 'sent'
				LIMIT 1`,
				msg.InReplyTo,
			).Scan(&campaignID, &leadID)

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
				var domain string
				db.QueryRow("SELECT domain FROM leads WHERE id = ?", leadID).Scan(&domain)

				if domain != "" {
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
// Returns the number of new bounces detected.
func ProcessBounces(db *sql.DB, gws GWSClient, accounts []Account) (int, error) {
	bouncesFound := 0
	lastPoll := GetLastPollAt(db)

	afterFilter := fmt.Sprintf("(from:mailer-daemon OR from:postmaster) after:%d", lastPoll.Unix())

	for _, account := range accounts {
		messages, err := gws.ListMessages(account.Email, afterFilter)
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
			var globalStatus string
			err := db.QueryRow("SELECT id, global_status FROM leads WHERE email = ?", bouncedEmail).Scan(&leadID, &globalStatus)
			if err == sql.ErrNoRows {
				continue // Not one of our leads
			}
			if err != nil {
				return bouncesFound, fmt.Errorf("looking up lead %s: %w", bouncedEmail, err)
			}

			// Dedup: skip if already bounced
			if globalStatus == "bounced" {
				continue
			}

			// Mark lead as globally bounced
			db.Exec("UPDATE leads SET global_status = 'bounced' WHERE id = ?", leadID)

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
func extractBouncedEmail(snippet, subject string) string {
	combined := snippet + " " + subject

	words := strings.Fields(combined)
	for _, word := range words {
		word = strings.Trim(word, "<>()[],.;:\"'")
		if isLikelyBouncedEmail(word) {
			return strings.ToLower(word)
		}
	}

	return ""
}

func isLikelyBouncedEmail(s string) bool {
	if strings.Contains(s, " ") {
		return false
	}
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	if !strings.Contains(parts[1], ".") {
		return false
	}
	// Filter out mailer-daemon and postmaster addresses (these are senders, not bounced recipients)
	local := strings.ToLower(parts[0])
	if local == "mailer-daemon" || local == "postmaster" {
		return false
	}
	return true
}
