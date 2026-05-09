package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// GetLastPollAt returns the last poll timestamp, or a default (24h ago).
func GetLastPollAt(db *sql.DB) time.Time {
	var val string
	err := queryRowDB(db, "SELECT value FROM kv WHERE key = 'last_poll_at'").Scan(&val)
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
	if _, err := execDB(db, `INSERT INTO kv (key, value) VALUES ('last_poll_at', ?)
		ON CONFLICT(key) DO UPDATE SET value = ?`,
		t.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("failed to update last_poll_at", "error", err)
	}
}

// IsUnsubscribeRequest checks if a message appears to be an unsubscribe request
// based on its subject and snippet text.
func IsUnsubscribeRequest(subject, snippet string) bool {
	text := strings.ToLower(subject + " " + snippet)

	unsubPhrases := []string{
		"unsubscribe",
		"stop emailing",
		"stop sending",
		"remove me",
		"opt out",
		"opt-out",
		"take me off",
		"don't contact",
		"do not contact",
		"don't email",
		"do not email",
		"stop contacting",
	}

	for _, phrase := range unsubPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}

	return false
}

// ProcessReplies checks inbox messages for replies to our sent emails.
// Returns the number of new replies and unsubscribes detected.
func ProcessReplies(db *sql.DB, gws GWSClient, accounts []Account) (replies int, unsubscribes int, err error) {
	lastPoll := GetLastPollAt(db)

	// Gmail 'after:' uses epoch seconds
	afterFilter := fmt.Sprintf("in:inbox after:%d", lastPoll.Unix())

	return processReplyMessages(db, accounts, func(account Account) ([]GWSMessage, error) {
		return gws.ListMessages(account.Email, afterFilter)
	})
}

// ProcessIMAPReplies checks IMAP inbox messages for replies to sent SMTP/IMAP emails.
func ProcessIMAPReplies(db *sql.DB, imap IMAPMessageLister, accounts []Account) (replies int, unsubscribes int, err error) {
	lastPoll := GetLastPollAt(db)

	return processReplyMessages(db, accounts, func(account Account) ([]GWSMessage, error) {
		return imap.ListMessages(account, lastPoll, false)
	})
}

func processReplyMessages(db *sql.DB, accounts []Account, listMessages func(Account) ([]GWSMessage, error)) (replies int, unsubscribes int, err error) {
	for _, account := range accounts {
		messages, err := listMessages(account)
		if err != nil {
			return replies, unsubscribes, fmt.Errorf("listing messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			// Dedup: skip if we already recorded this message as a reply or unsubscribe event
			var existing int
			queryRowDB(db, "SELECT COUNT(*) FROM events WHERE message_id = ? AND type IN ('reply', 'unsubscribe')",
				msg.ID).Scan(&existing)
			if existing > 0 {
				continue
			}

			var campaignID, leadID int64
			matched := false

			// Strategy 1: In-Reply-To header matching
			if msg.InReplyTo != "" {
				err := queryRowDB(db, `
					SELECT e.campaign_id, e.lead_id
					FROM events e
					WHERE e.message_id = ? AND e.type = 'sent'
					LIMIT 1`,
					msg.InReplyTo,
				).Scan(&campaignID, &leadID)

				if err == nil {
					matched = true
				} else if err != sql.ErrNoRows {
					return replies, unsubscribes, fmt.Errorf("looking up event for In-Reply-To %s: %w", msg.InReplyTo, err)
				}
			}

			// Strategy 2: Thread-ID matching (catches replies from shared inboxes,
			// forwarded addresses, or mail clients that don't set In-Reply-To)
			if !matched && msg.ThreadID != "" {
				err := queryRowDB(db, `
					SELECT e.campaign_id, e.lead_id
					FROM events e
					WHERE e.thread_id = ? AND e.type = 'sent'
					LIMIT 1`,
					msg.ThreadID,
				).Scan(&campaignID, &leadID)

				if err == nil {
					matched = true
					slog.Info("reply matched via thread-ID",
						"thread_id", msg.ThreadID, "message_id", msg.ID,
						"campaign_id", campaignID, "lead_id", leadID)
				}
			}

			if !matched {
				continue
			}

			// Check if this is an unsubscribe request
			if IsUnsubscribeRequest(msg.Subject, msg.Snippet) {
				// Record unsubscribe event
				if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
					VALUES (?, ?, ?, 'unsubscribe', 0, ?, ?)`,
					campaignID, leadID, account.ID, msg.ID, msg.ThreadID); err != nil {
					slog.Warn("failed to insert unsubscribe event",
						"campaign_id", campaignID, "lead_id", leadID,
						"message_id", msg.ID, "error", err)
				}
				if err := insertInboundEmailMessage(db, account, campaignID, leadID, msg, EmailMessageTypeUnsubscribe); err != nil {
					slog.Warn("failed to insert unsubscribe email message snapshot",
						"campaign_id", campaignID, "lead_id", leadID,
						"message_id", msg.ID, "error", err)
				}

				// Blacklist the lead globally (cancels all pending sends across all campaigns)
				var leadEmail string
				queryRowDB(db, "SELECT email FROM leads WHERE id = ?", leadID).Scan(&leadEmail)
				if leadEmail != "" {
					if _, err := BlacklistLead(db, leadEmail); err != nil {
						slog.Warn("failed to blacklist unsubscribed lead",
							"lead_email", leadEmail, "error", err)
					}
				}

				slog.Info("unsubscribe detected",
					"campaign_id", campaignID, "lead_id", leadID,
					"lead_email", leadEmail, "message_id", msg.ID)
				unsubscribes++
				continue
			}

			// Record the reply event
			if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
				VALUES (?, ?, ?, 'reply', 0, ?, ?)`,
				campaignID, leadID, account.ID, msg.ID, msg.ThreadID); err != nil {
				slog.Warn("failed to insert reply event",
					"campaign_id", campaignID, "lead_id", leadID,
					"message_id", msg.ID, "error", err)
			}
			if err := insertInboundEmailMessage(db, account, campaignID, leadID, msg, EmailMessageTypeReply); err != nil {
				slog.Warn("failed to insert reply email message snapshot",
					"campaign_id", campaignID, "lead_id", leadID,
					"message_id", msg.ID, "error", err)
			}

			// Update lead status
			if _, err := execDB(db, "UPDATE campaign_leads SET status = 'replied' WHERE campaign_id = ? AND lead_id = ? AND status = 'active'",
				campaignID, leadID); err != nil {
				slog.Warn("failed to update campaign_lead to replied",
					"campaign_id", campaignID, "lead_id", leadID, "error", err)
			}

			// Skip remaining scheduled sends
			if _, err := execDB(db, "UPDATE scheduled_sends SET status = 'skipped' WHERE campaign_id = ? AND lead_id = ? AND status = 'pending'",
				campaignID, leadID); err != nil {
				slog.Warn("failed to skip remaining sends after reply",
					"campaign_id", campaignID, "lead_id", leadID, "error", err)
			}

			// Check stop_on_domain_reply
			var stopOnDomainReply int
			queryRowDB(db, "SELECT stop_on_domain_reply FROM campaigns WHERE id = ?", campaignID).Scan(&stopOnDomainReply)

			if stopOnDomainReply != 0 {
				var domain string
				queryRowDB(db, "SELECT domain FROM leads WHERE id = ?", leadID).Scan(&domain)

				if domain != "" {
					if _, err := execDB(db, `
						UPDATE scheduled_sends SET status = 'skipped'
						WHERE campaign_id = ?
						AND lead_id IN (SELECT id FROM leads WHERE domain = ? AND id != ?)
						AND status = 'pending'`,
						campaignID, domain, leadID); err != nil {
						slog.Warn("failed to skip domain sends",
							"campaign_id", campaignID, "domain", domain, "error", err)
					}

					if _, err := execDB(db, `
						UPDATE campaign_leads SET status = 'paused'
						WHERE campaign_id = ?
						AND lead_id IN (SELECT id FROM leads WHERE domain = ? AND id != ?)
						AND status = 'active'`,
						campaignID, domain, leadID); err != nil {
						slog.Warn("failed to pause domain leads",
							"campaign_id", campaignID, "domain", domain, "error", err)
					}
				}
			}

			replies++
		}
	}

	return replies, unsubscribes, nil
}

func insertInboundEmailMessage(db *sql.DB, account Account, campaignID, leadID int64, msg GWSMessage, messageType string) error {
	return insertEmailMessage(db, EmailMessage{
		CampaignID: campaignID,
		LeadID:     leadID,
		AccountID:  account.ID,
		Direction:  EmailMessageDirectionInbound,
		Type:       messageType,
		StepNumber: 0,
		MessageID:  msg.ID,
		ThreadID:   msg.ThreadID,
		InReplyTo:  msg.InReplyTo,
		FromEmail:  msg.From,
		ToEmails:   msg.To,
		Subject:    msg.Subject,
		TextBody:   textBodyForInboundSnapshot(msg),
		HTMLBody:   msg.HTMLBody,
		Snippet:    msg.Snippet,
		RawHeaders: emailHeadersJSON(msg.Headers),
		OccurredAt: time.Now().UTC(),
	})
}

// ProcessBounces checks inbox messages for bounce NDRs.
// Uses two strategies:
//  1. Thread matching — if the NDR shares a thread_id with a sent email, we know the lead
//  2. Snippet/header parsing — fallback: extract bounced email from NDR text
//
// Returns the number of new bounces detected.
func ProcessBounces(db *sql.DB, gws GWSClient, accounts []Account) (int, error) {
	lastPoll := GetLastPollAt(db)

	afterFilter := fmt.Sprintf("(from:mailer-daemon OR from:postmaster) after:%d", lastPoll.Unix())

	return processBounceMessages(db, accounts, func(account Account) ([]GWSMessage, error) {
		return gws.ListMessages(account.Email, afterFilter, true)
	})
}

// ProcessIMAPBounces checks IMAP messages for bounce NDRs.
func ProcessIMAPBounces(db *sql.DB, imap IMAPMessageLister, accounts []Account) (int, error) {
	lastPoll := GetLastPollAt(db)

	return processBounceMessages(db, accounts, func(account Account) ([]GWSMessage, error) {
		return imap.ListMessages(account, lastPoll, true)
	})
}

func processBounceMessages(db *sql.DB, accounts []Account, listMessages func(Account) ([]GWSMessage, error)) (int, error) {
	bouncesFound := 0
	for _, account := range accounts {
		messages, err := listMessages(account)
		if err != nil {
			return bouncesFound, fmt.Errorf("listing bounce messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			if !isBounceMessage(msg) {
				continue
			}
			leadID, found := resolveBounceToLead(db, msg)
			if !found {
				continue
			}

			// Dedup: skip if already bounced
			var globalStatus string
			queryRowDB(db, "SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
			if globalStatus == "bounced" {
				continue
			}

			// Mark lead as globally bounced
			if _, err := execDB(db, "UPDATE leads SET global_status = 'bounced' WHERE id = ?", leadID); err != nil {
				slog.Warn("failed to mark lead as bounced",
					"lead_id", leadID, "error", err)
			}

			// Update all campaign_leads
			if _, err := execDB(db, "UPDATE campaign_leads SET status = 'bounced' WHERE lead_id = ? AND status IN ('active', 'pending')", leadID); err != nil {
				slog.Warn("failed to update campaign_leads to bounced",
					"lead_id", leadID, "error", err)
			}

			// Skip all pending sends across all campaigns
			if _, err := execDB(db, "UPDATE scheduled_sends SET status = 'skipped' WHERE lead_id = ? AND status = 'pending'", leadID); err != nil {
				slog.Warn("failed to skip pending sends for bounced lead",
					"lead_id", leadID, "error", err)
			}

			bouncesFound++
		}
	}

	return bouncesFound, nil
}

func isBounceMessage(msg GWSMessage) bool {
	if strings.TrimSpace(msg.Headers["X-Failed-Recipients"]) != "" {
		return true
	}

	from := strings.ToLower(msg.From + " " + msg.Headers["From"])
	if strings.Contains(from, "mailer-daemon") || strings.Contains(from, "postmaster") {
		return true
	}

	subject := strings.ToLower(msg.Subject)
	bounceSubjects := []string{
		"delivery status notification",
		"delivery failed",
		"delivery failure",
		"undelivered mail",
		"undeliverable",
		"address not found",
		"mail delivery failed",
		"returned mail",
	}
	for _, phrase := range bounceSubjects {
		if strings.Contains(subject, phrase) {
			return true
		}
	}
	return false
}

// resolveBounceToLead identifies which lead a bounce NDR belongs to.
// Strategy 1: thread matching — NDR is in the same Gmail thread as our sent email.
// Strategy 2: X-Failed-Recipients header (if available).
// Strategy 3: snippet/subject text parsing (fallback).
func resolveBounceToLead(db *sql.DB, msg GWSMessage) (leadID int64, found bool) {
	// Strategy 1: Thread matching
	if msg.ThreadID != "" {
		var id int64
		err := queryRowDB(db, `
			SELECT e.lead_id FROM events e
			WHERE e.thread_id = ? AND e.type = 'sent'
			LIMIT 1`, msg.ThreadID).Scan(&id)
		if err == nil {
			return id, true
		}
	}

	// Strategy 2: X-Failed-Recipients header
	if failedRecip, ok := msg.Headers["X-Failed-Recipients"]; ok && failedRecip != "" {
		email := strings.ToLower(strings.TrimSpace(failedRecip))
		var id int64
		err := queryRowDB(db, "SELECT id FROM leads WHERE email = ?", email).Scan(&id)
		if err == nil {
			return id, true
		}
	}

	// Strategy 3: Snippet/subject text parsing (fallback)
	bouncedEmail := extractBouncedEmail(msg.Snippet, msg.Subject)
	if bouncedEmail != "" {
		var id int64
		err := queryRowDB(db, "SELECT id FROM leads WHERE email = ?", bouncedEmail).Scan(&id)
		if err == nil {
			return id, true
		}
	}

	return 0, false
}

// extractBouncedEmail attempts to extract a bounced email address from NDR text.
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
	local := strings.ToLower(parts[0])
	if local == "mailer-daemon" || local == "postmaster" {
		return false
	}
	return true
}
