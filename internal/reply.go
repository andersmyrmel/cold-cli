package internal

import (
	"database/sql"
	"errors"
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

func replyPollCursorKey(account Account) string {
	provider := strings.TrimSpace(account.Provider)
	if provider == "" {
		provider = AccountProviderGWS
	}
	return fmt.Sprintf("reply_poll_at:%s:%d", provider, account.ID)
}

func getReplyPollAt(db *sql.DB, account Account) time.Time {
	var value string
	if err := queryRowDB(db, "SELECT value FROM kv WHERE key = ?", replyPollCursorKey(account)).Scan(&value); err == nil {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed
		}
	}
	// Preserve the legacy shared cursor during rollout. Once this account polls
	// successfully it receives its own independent cursor.
	return GetLastPollAt(db)
}

func setReplyPollAt(db *sql.DB, account Account, at time.Time) error {
	value := at.UTC().Format(time.RFC3339)
	_, err := execDB(db, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = ?`, replyPollCursorKey(account), value, value)
	return err
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
	result, err := processGWSReplyMessages(db, gws, accounts)
	if err != nil {
		return 0, 0, err
	}
	return result.Replies, result.Unsubscribes, nil
}

func processGWSReplyMessages(db *sql.DB, gws GWSClient, accounts []Account) (replyPollResult, error) {
	return processReplyAccounts(db, accounts, func(account Account, since time.Time) ([]GWSMessage, error) {
		// Search all mail addressed to this account, not just INBOX. Manual
		// replies/archive actions can remove INBOX before the next tick.
		query := fmt.Sprintf("to:%s -from:%s after:%d", account.Email, account.Email, since.Unix())
		return gws.ListMessages(account.Email, query)
	})
}

// ProcessIMAPReplies checks IMAP inbox messages for replies to sent SMTP/IMAP emails.
func ProcessIMAPReplies(db *sql.DB, imap IMAPMessageLister, accounts []Account) (replies int, unsubscribes int, err error) {
	result, err := processIMAPReplyMessages(db, imap, accounts)
	if err != nil {
		return 0, 0, err
	}
	return result.Replies, result.Unsubscribes, nil
}

func processIMAPReplyMessages(db *sql.DB, imap IMAPMessageLister, accounts []Account) (replyPollResult, error) {
	return processReplyAccounts(db, accounts, func(account Account, since time.Time) ([]GWSMessage, error) {
		return imap.ListMessages(account, since, false)
	})
}

func processReplyAccounts(db *sql.DB, accounts []Account, listMessages func(Account, time.Time) ([]GWSMessage, error)) (replyPollResult, error) {
	var total replyPollResult
	var errs []error
	pollStartedAt := time.Now().UTC()
	for _, account := range accounts {
		pollSince := getReplyPollAt(db, account).Add(-replyPollOverlap)
		result, err := processReplyMessages(db, []Account{account}, func(current Account) ([]GWSMessage, error) {
			return listMessages(current, pollSince)
		})
		total.Replies += result.Replies
		total.Unsubscribes += result.Unsubscribes
		total.Bounces += result.Bounces
		total.AutoReplies += result.AutoReplies
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := setReplyPollAt(db, account, pollStartedAt); err != nil {
			errs = append(errs, fmt.Errorf("updating reply cursor for %s: %w", account.Email, err))
		}
	}
	return total, errors.Join(errs...)
}

type replyPollResult struct {
	Replies      int
	Unsubscribes int
	Bounces      int
	AutoReplies  int
}

const replyPollOverlap = 2 * time.Hour

type inboundClassification string

const (
	inboundClassificationReply       inboundClassification = "reply"
	inboundClassificationUnsubscribe inboundClassification = "unsubscribe"
	inboundClassificationBounce      inboundClassification = "bounce"
	inboundClassificationAutoReply   inboundClassification = "auto_reply"
)

func processReplyMessages(db *sql.DB, accounts []Account, listMessages func(Account) ([]GWSMessage, error)) (replyPollResult, error) {
	var result replyPollResult
	for _, account := range accounts {
		messages, err := listMessages(account)
		if err != nil {
			return result, fmt.Errorf("listing messages for %s: %w", account.Email, err)
		}

		for _, msg := range messages {
			// Dedup: skip if we already recorded this inbound message classification.
			var existing int
			queryRowDB(db, "SELECT COUNT(*) FROM events WHERE message_id = ? AND type IN ('reply', 'unsubscribe', 'bounce', 'auto_reply')",
				msg.ID).Scan(&existing)
			if existing > 0 {
				continue
			}

			var campaignID, leadID, threadAccountID int64
			matched := false

			// Strategy 1: In-Reply-To header matching
			if msg.InReplyTo != "" {
				err := queryRowDB(db, `
					SELECT e.campaign_id, e.lead_id, e.account_id
					FROM events e
					WHERE e.message_id = ? AND e.type IN ('sent', 'manual_reply')
					LIMIT 1`,
					msg.InReplyTo,
				).Scan(&campaignID, &leadID, &threadAccountID)

				if err == nil {
					matched = true
				} else if err != sql.ErrNoRows {
					return result, fmt.Errorf("looking up event for In-Reply-To %s: %w", msg.InReplyTo, err)
				}
			}

			// Strategy 2: Thread-ID matching (catches replies from shared inboxes,
			// forwarded addresses, or mail clients that don't set In-Reply-To)
			if !matched && msg.ThreadID != "" {
				err := queryRowDB(db, `
					SELECT e.campaign_id, e.lead_id, e.account_id
					FROM events e
					WHERE e.thread_id = ? AND e.type IN ('sent', 'manual_reply')
					LIMIT 1`,
					msg.ThreadID,
				).Scan(&campaignID, &leadID, &threadAccountID)

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
			threadAccount := account
			if threadAccountID != 0 && threadAccountID != account.ID {
				loadedAccount, err := getAccountByID(db, threadAccountID)
				if err != nil {
					return result, err
				}
				threadAccount = loadedAccount
			}

			switch classifyInboundMessage(msg) {
			case inboundClassificationBounce:
				if recordBounceForLead(db, threadAccount, campaignID, leadID, msg) {
					result.Bounces++
				}
				continue
			case inboundClassificationAutoReply:
				if recordAutoReply(db, threadAccount, campaignID, leadID, msg) {
					result.AutoReplies++
				}
				continue
			case inboundClassificationUnsubscribe:
				occurredAt := inboundEmailOccurredAt(msg)

				// Record unsubscribe event
				if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
					VALUES (?, ?, ?, 'unsubscribe', 0, ?, ?, ?)`,
					campaignID, leadID, threadAccount.ID, msg.ID, msg.ThreadID, occurredAt); err != nil {
					slog.Warn("failed to insert unsubscribe event",
						"campaign_id", campaignID, "lead_id", leadID,
						"message_id", msg.ID, "error", err)
				}
				if err := insertInboundEmailMessage(db, threadAccount, campaignID, leadID, msg, EmailMessageTypeUnsubscribe); err != nil {
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
				result.Unsubscribes++
				continue
			}

			occurredAt := inboundEmailOccurredAt(msg)

			// Record the reply event
			if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
				VALUES (?, ?, ?, 'reply', 0, ?, ?, ?)`,
				campaignID, leadID, threadAccount.ID, msg.ID, msg.ThreadID, occurredAt); err != nil {
				slog.Warn("failed to insert reply event",
					"campaign_id", campaignID, "lead_id", leadID,
					"message_id", msg.ID, "error", err)
			}
			if err := insertInboundEmailMessage(db, threadAccount, campaignID, leadID, msg, EmailMessageTypeReply); err != nil {
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

			result.Replies++
		}
	}

	return result, nil
}

func inboundEmailOccurredAt(msg GWSMessage) time.Time {
	if !msg.Date.IsZero() {
		return msg.Date.UTC()
	}
	return time.Now().UTC()
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
		OccurredAt: inboundEmailOccurredAt(msg),
	})
}

func classifyInboundMessage(msg GWSMessage) inboundClassification {
	if isBounceMessage(msg) {
		return inboundClassificationBounce
	}
	if isAutoReplyMessage(msg) {
		return inboundClassificationAutoReply
	}
	if IsUnsubscribeRequest(msg.Subject, msg.Snippet) {
		return inboundClassificationUnsubscribe
	}
	return inboundClassificationReply
}

func recordAutoReply(db *sql.DB, account Account, campaignID, leadID int64, msg GWSMessage) bool {
	if eventExistsForMessage(db, msg.ID, EmailMessageTypeAutoReply) {
		return false
	}

	occurredAt := inboundEmailOccurredAt(msg)
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
		VALUES (?, ?, ?, 'auto_reply', 0, ?, ?, ?)`,
		campaignID, leadID, account.ID, msg.ID, msg.ThreadID, occurredAt); err != nil {
		slog.Warn("failed to insert auto_reply event",
			"campaign_id", campaignID, "lead_id", leadID,
			"message_id", msg.ID, "error", err)
		return false
	}
	if err := insertInboundEmailMessage(db, account, campaignID, leadID, msg, EmailMessageTypeAutoReply); err != nil {
		slog.Warn("failed to insert auto_reply email message snapshot",
			"campaign_id", campaignID, "lead_id", leadID,
			"message_id", msg.ID, "error", err)
	}

	slog.Info("auto-reply ignored for reply stats",
		"campaign_id", campaignID, "lead_id", leadID, "message_id", msg.ID)
	return true
}

func recordBounceForLead(db *sql.DB, account Account, campaignID, leadID int64, msg GWSMessage) bool {
	if leadID == 0 {
		return false
	}

	var globalStatus string
	queryRowDB(db, "SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
	if globalStatus == "bounced" {
		return false
	}

	campaignID, account.ID = resolveBounceEventContext(db, account, campaignID, leadID, msg)

	if _, err := execDB(db, "UPDATE leads SET global_status = 'bounced' WHERE id = ?", leadID); err != nil {
		slog.Warn("failed to mark lead as bounced",
			"lead_id", leadID, "error", err)
	}

	if _, err := execDB(db, "UPDATE campaign_leads SET status = 'bounced' WHERE lead_id = ? AND status IN ('active', 'pending')", leadID); err != nil {
		slog.Warn("failed to update campaign_leads to bounced",
			"lead_id", leadID, "error", err)
	}

	if _, err := execDB(db, "UPDATE scheduled_sends SET status = 'skipped' WHERE lead_id = ? AND status = 'pending'", leadID); err != nil {
		slog.Warn("failed to skip pending sends for bounced lead",
			"lead_id", leadID, "error", err)
	}

	if campaignID != 0 && account.ID != 0 && !eventExistsForMessage(db, msg.ID, EmailMessageTypeBounce) {
		occurredAt := inboundEmailOccurredAt(msg)
		if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
			VALUES (?, ?, ?, 'bounce', 0, ?, ?, ?)`,
			campaignID, leadID, account.ID, msg.ID, msg.ThreadID, occurredAt); err != nil {
			slog.Warn("failed to insert bounce event",
				"campaign_id", campaignID, "lead_id", leadID,
				"message_id", msg.ID, "error", err)
		}
		if err := insertInboundEmailMessage(db, account, campaignID, leadID, msg, EmailMessageTypeBounce); err != nil {
			slog.Warn("failed to insert bounce email message snapshot",
				"campaign_id", campaignID, "lead_id", leadID,
				"message_id", msg.ID, "error", err)
		}
	}

	slog.Info("bounce detected",
		"campaign_id", campaignID, "lead_id", leadID, "message_id", msg.ID)
	return true
}

func resolveBounceEventContext(db *sql.DB, account Account, campaignID, leadID int64, msg GWSMessage) (int64, int64) {
	accountID := account.ID
	if campaignID != 0 && accountID != 0 {
		return campaignID, accountID
	}

	if msg.ThreadID != "" {
		var sentCampaignID, sentAccountID int64
		err := queryRowDB(db, `
			SELECT e.campaign_id, e.account_id
			FROM events e
			WHERE e.thread_id = ? AND e.type IN ('sent', 'manual_reply')
			ORDER BY e.timestamp DESC, e.id DESC
			LIMIT 1`, msg.ThreadID).Scan(&sentCampaignID, &sentAccountID)
		if err == nil {
			if campaignID == 0 {
				campaignID = sentCampaignID
			}
			if sentAccountID != 0 {
				accountID = sentAccountID
			}
		}
	}

	if leadID != 0 && (campaignID == 0 || accountID == 0) {
		var sentCampaignID, sentAccountID int64
		err := queryRowDB(db, `
			SELECT e.campaign_id, e.account_id
			FROM events e
			WHERE e.lead_id = ? AND e.type IN ('sent', 'manual_reply')
			ORDER BY e.timestamp DESC, e.id DESC
			LIMIT 1`, leadID).Scan(&sentCampaignID, &sentAccountID)
		if err == nil {
			if campaignID == 0 {
				campaignID = sentCampaignID
			}
			if sentAccountID != 0 {
				accountID = sentAccountID
			}
		}
	}

	if leadID != 0 && campaignID == 0 {
		_ = queryRowDB(db, `
			SELECT campaign_id
			FROM campaign_leads
			WHERE lead_id = ?
			ORDER BY campaign_id
			LIMIT 1`, leadID).Scan(&campaignID)
	}

	return campaignID, accountID
}

func eventExistsForMessage(db *sql.DB, messageID, eventType string) bool {
	if strings.TrimSpace(messageID) == "" {
		return false
	}
	var existing int
	queryRowDB(db, "SELECT COUNT(*) FROM events WHERE message_id = ? AND type = ?", messageID, eventType).Scan(&existing)
	return existing > 0
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

			if recordBounceForLead(db, account, 0, leadID, msg) {
				bouncesFound++
			}
		}
	}

	return bouncesFound, nil
}

func isBounceMessage(msg GWSMessage) bool {
	if strings.TrimSpace(headerValue(msg.Headers, "X-Failed-Recipients")) != "" {
		return true
	}

	if isDeliveryStatusReport(msg) {
		return true
	}

	from := classificationText(msg.From + " " + headerValue(msg.Headers, "From"))
	if strings.Contains(from, "mailer-daemon") || strings.Contains(from, "postmaster") {
		return true
	}

	text := classificationText(msg.Subject + " " + msg.Snippet + " " + msg.TextBody + " " + msg.HTMLBody)
	bouncePhrases := []string{
		"delivery status notification",
		"delivery failed",
		"delivery failure",
		"delivery has failed",
		"undelivered mail",
		"undelivered mail returned to sender",
		"undeliverable",
		"address not found",
		"mail delivery failed",
		"message not delivered",
		"message wasn't delivered",
		"message could not be delivered",
		"returned mail",
		"550 5.1.1",
		"nosuchuser",
		"no such user",
		"user unknown",
		"recipient address rejected",
		"mailbox unavailable",
	}
	for _, phrase := range bouncePhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func isAutoReplyMessage(msg GWSMessage) bool {
	if isDispositionNotificationReport(msg) {
		return true
	}

	autoSubmitted := strings.ToLower(strings.TrimSpace(headerValue(msg.Headers, "Auto-Submitted")))
	if autoSubmitted != "" && autoSubmitted != "no" {
		return true
	}

	autoHeaders := []string{
		"X-Autoreply",
		"X-Autorespond",
		"X-Auto-Reply",
		"X-Auto-Response",
	}
	for _, name := range autoHeaders {
		if strings.TrimSpace(headerValue(msg.Headers, name)) != "" {
			return true
		}
	}

	precedence := classificationText(headerValue(msg.Headers, "Precedence"))
	if precedence == "bulk" || precedence == "auto_reply" || precedence == "auto-reply" {
		return true
	}

	text := classificationText(msg.Subject + " " + msg.Snippet + " " + msg.TextBody + " " + msg.HTMLBody)
	autoPhrases := []string{
		"auto reply",
		"auto-reply",
		"automatic reply",
		"automated reply",
		"automatic response",
		"out of office",
		"out of the office",
		"away from the office",
		"thank you for contacting us",
		"thanks for contacting us",
		"ticket submitted",
		"ticket has been submitted",
		"your ticket has been submitted",
		"we'll pick up your ticket",
		"we will pick up your ticket",
		"we have received your request",
		"we've received your request",
		"we received your request",
		"your request has been received",
		"support ticket",
		"read receipt",
		"return receipt",
		"disposition notification",
	}
	for _, phrase := range autoPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}

	return false
}

func isDeliveryStatusReport(msg GWSMessage) bool {
	contentType := classificationText(headerValue(msg.Headers, "Content-Type"))
	if strings.Contains(contentType, "message/delivery-status") {
		return true
	}
	if strings.Contains(contentType, "multipart/report") && reportTypeValue(contentType) == "delivery-status" {
		return true
	}
	for _, mimeType := range messageMimeTypes(msg) {
		if strings.Contains(classificationText(mimeType), "message/delivery-status") {
			return true
		}
	}
	return false
}

func isDispositionNotificationReport(msg GWSMessage) bool {
	contentType := classificationText(headerValue(msg.Headers, "Content-Type"))
	if strings.Contains(contentType, "message/disposition-notification") {
		return true
	}
	if strings.Contains(contentType, "multipart/report") && reportTypeValue(contentType) == "disposition-notification" {
		return true
	}
	for _, mimeType := range messageMimeTypes(msg) {
		if strings.Contains(classificationText(mimeType), "message/disposition-notification") {
			return true
		}
	}
	return false
}

func messageMimeTypes(msg GWSMessage) []string {
	mimeTypes := []string{msg.MimeType}
	mimeTypes = append(mimeTypes, msg.PartMimeTypes...)
	return mimeTypes
}

func reportTypeValue(contentType string) string {
	for _, part := range strings.Split(contentType, ";") {
		part = strings.TrimSpace(part)
		key, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(key) != "report-type" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

func headerValue(headers map[string]string, names ...string) string {
	for _, name := range names {
		for key, value := range headers {
			if strings.EqualFold(key, name) {
				return value
			}
		}
	}
	return ""
}

func classificationText(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer(
		"\u2018", "'",
		"\u2019", "'",
		"\u201c", `"`,
		"\u201d", `"`,
		"\u00a0", " ",
		"\r", " ",
		"\n", " ",
		"\t", " ",
	).Replace(value)
	return strings.Join(strings.Fields(value), " ")
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
			WHERE e.thread_id = ? AND e.type IN ('sent', 'manual_reply')
			LIMIT 1`, msg.ThreadID).Scan(&id)
		if err == nil {
			return id, true
		}
	}

	// Strategy 2: X-Failed-Recipients header
	if failedRecip := headerValue(msg.Headers, "X-Failed-Recipients"); failedRecip != "" {
		email := strings.ToLower(strings.TrimSpace(failedRecip))
		var id int64
		err := queryRowDB(db, "SELECT id FROM leads WHERE email = ?", email).Scan(&id)
		if err == nil {
			return id, true
		}
	}

	// Strategy 3: Snippet/subject text parsing (fallback)
	bouncedEmail := extractBouncedEmail(msg.Snippet+" "+msg.TextBody, msg.Subject)
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
