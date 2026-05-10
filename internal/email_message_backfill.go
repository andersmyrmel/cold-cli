package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type BackfillEmailMessagesConfig struct {
	DB          *sql.DB
	GWS         GWSClient
	Since       time.Time
	Limit       int
	DryRun      bool
	IncludeSent bool
}

type BackfillEmailMessagesResult struct {
	Scanned     int  `json:"scanned"`
	Backfilled  int  `json:"backfilled"`
	Sent        int  `json:"sent"`
	Inbound     int  `json:"inbound"`
	Skipped     int  `json:"skipped"`
	Unsupported int  `json:"unsupported"`
	Failed      int  `json:"failed"`
	DryRun      bool `json:"dry_run"`
}

type emailMessageBackfillEvent struct {
	ID           int64
	CampaignID   int64
	LeadID       int64
	AccountID    int64
	AccountEmail string
	Provider     string
	Type         string
	StepNumber   int
	MessageID    string
	ThreadID     string
	Timestamp    time.Time
}

type emailMessageBackfillAccount struct {
	ID       int64
	Email    string
	Provider string
}

func BackfillEmailMessages(cfg BackfillEmailMessagesConfig) (*BackfillEmailMessagesResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	result := &BackfillEmailMessagesResult{DryRun: cfg.DryRun}

	events, err := loadInboundEventsForEmailMessageBackfill(cfg.DB, cfg.Since, cfg.Limit, !cfg.IncludeSent)
	if err != nil {
		return nil, err
	}
	result.Scanned = len(events)

	seenSentThreads := map[string]struct{}{}
	for _, event := range events {
		threadAccount, foundThreadAccount, err := resolveBackfillThreadAccount(cfg.DB, event)
		if err != nil {
			return nil, err
		}
		storeEvent := event
		if foundThreadAccount {
			storeEvent.AccountID = threadAccount.ID
			storeEvent.AccountEmail = threadAccount.Email
			storeEvent.Provider = threadAccount.Provider
			if err := repairBackfilledThreadAccountIDs(cfg.DB, event, threadAccount.ID, cfg.DryRun); err != nil {
				return nil, err
			}
		}

		inboundExists, err := emailMessageSnapshotExists(cfg.DB, storeEvent.CampaignID, storeEvent.LeadID, storeEvent.MessageID)
		if err != nil {
			return nil, err
		}

		backfilled := false
		if !inboundExists {
			backfilled, err = backfillInboundEventEmailMessage(cfg, storeEvent)
		}
		switch {
		case err != nil:
			result.Failed++
			slog.Warn("failed to backfill inbound email message",
				"event_id", event.ID, "message_id", event.MessageID, "error", err)
		case backfilled:
			result.Backfilled++
			result.Inbound++
		default:
			result.Skipped++
		}

		if !cfg.IncludeSent {
			continue
		}
		threadKey := fmt.Sprintf("%d:%d:%s", storeEvent.CampaignID, storeEvent.LeadID, storeEvent.ThreadID)
		if _, ok := seenSentThreads[threadKey]; ok {
			continue
		}
		seenSentThreads[threadKey] = struct{}{}

		sentBackfilled, sentUnsupported, sentFailed, err := backfillRelatedSentEmailMessages(cfg, storeEvent)
		if err != nil {
			return nil, err
		}
		result.Backfilled += sentBackfilled
		result.Sent += sentBackfilled
		result.Unsupported += sentUnsupported
		result.Failed += sentFailed

		threadBackfilled, threadInbound, threadSent, threadUnsupported, threadFailed, err := backfillProviderThreadEmailMessages(cfg, storeEvent)
		if err != nil {
			return nil, err
		}
		result.Backfilled += threadBackfilled
		result.Inbound += threadInbound
		result.Sent += threadSent
		result.Unsupported += threadUnsupported
		result.Failed += threadFailed
	}

	return result, nil
}

func loadInboundEventsForEmailMessageBackfill(db *sql.DB, since time.Time, limit int, missingOnly bool) ([]emailMessageBackfillEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	query := `
		SELECT
			e.id,
			e.campaign_id,
			e.lead_id,
			e.account_id,
			a.email,
			a.provider,
			e.type,
			e.step_number,
			e.message_id,
			e.thread_id,
			e.timestamp
		FROM events e
		JOIN accounts a ON a.id = e.account_id
		LEFT JOIN email_messages em ON em.campaign_id = e.campaign_id
			AND em.lead_id = e.lead_id
			AND em.message_id = e.message_id
		WHERE e.type IN ('reply', 'unsubscribe')
			AND e.message_id <> ''
			AND e.thread_id <> ''`
	args := []any{}
	if missingOnly {
		query += " AND em.id IS NULL"
	}
	if !since.IsZero() {
		query += " AND e.timestamp >= ?"
		args = append(args, since.UTC())
	}
	query += " ORDER BY e.timestamp DESC, e.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("loading events missing email messages: %w", err)
	}
	defer rows.Close()

	var events []emailMessageBackfillEvent
	for rows.Next() {
		event, err := scanBackfillEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func emailMessageSnapshotExists(db *sql.DB, campaignID, leadID int64, messageID string) (bool, error) {
	if strings.TrimSpace(messageID) == "" {
		return false, nil
	}
	var id int64
	err := queryRowDB(db, `
		SELECT id
		FROM email_messages
		WHERE campaign_id = ? AND lead_id = ? AND message_id = ?
		LIMIT 1`, campaignID, leadID, messageID).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func resolveBackfillThreadAccount(db *sql.DB, event emailMessageBackfillEvent) (emailMessageBackfillAccount, bool, error) {
	if strings.TrimSpace(event.ThreadID) == "" {
		return emailMessageBackfillAccount{}, false, nil
	}

	var account emailMessageBackfillAccount
	err := queryRowDB(db, `
		SELECT a.id, a.email, a.provider
		FROM events e
		JOIN accounts a ON a.id = e.account_id
		WHERE e.campaign_id = ?
			AND e.lead_id = ?
			AND e.thread_id = ?
			AND e.type = 'sent'
		ORDER BY e.timestamp ASC, e.id ASC
		LIMIT 1`, event.CampaignID, event.LeadID, event.ThreadID).Scan(
		&account.ID,
		&account.Email,
		&account.Provider,
	)
	if err == nil {
		return account, true, nil
	}
	if err != sql.ErrNoRows {
		return emailMessageBackfillAccount{}, false, err
	}

	err = queryRowDB(db, `
		SELECT a.id, a.email, a.provider
		FROM email_messages em
		JOIN accounts a ON a.id = em.account_id
		WHERE em.campaign_id = ?
			AND em.lead_id = ?
			AND em.thread_id = ?
			AND em.direction = ?
		ORDER BY em.occurred_at ASC, em.id ASC
		LIMIT 1`, event.CampaignID, event.LeadID, event.ThreadID, EmailMessageDirectionOutbound).Scan(
		&account.ID,
		&account.Email,
		&account.Provider,
	)
	if err == sql.ErrNoRows {
		return emailMessageBackfillAccount{}, false, nil
	}
	if err != nil {
		return emailMessageBackfillAccount{}, false, err
	}
	return account, true, nil
}

func repairBackfilledThreadAccountIDs(db *sql.DB, event emailMessageBackfillEvent, accountID int64, dryRun bool) error {
	if dryRun || accountID == 0 || strings.TrimSpace(event.ThreadID) == "" || event.AccountID == accountID {
		return nil
	}

	if _, err := execDB(db, `
		UPDATE events
		SET account_id = ?
		WHERE campaign_id = ?
			AND lead_id = ?
			AND thread_id = ?
			AND type IN ('reply', 'unsubscribe')
			AND account_id <> ?`, accountID, event.CampaignID, event.LeadID, event.ThreadID, accountID); err != nil {
		return fmt.Errorf("repairing reply event account ids: %w", err)
	}

	if _, err := execDB(db, `
		UPDATE email_messages
		SET account_id = ?
		WHERE campaign_id = ?
			AND lead_id = ?
			AND thread_id = ?
			AND direction = ?
			AND account_id <> ?`, accountID, event.CampaignID, event.LeadID, event.ThreadID, EmailMessageDirectionInbound, accountID); err != nil {
		return fmt.Errorf("repairing inbound email message account ids: %w", err)
	}

	return nil
}

func scanBackfillEvent(scanner interface{ Scan(dest ...any) error }) (emailMessageBackfillEvent, error) {
	var event emailMessageBackfillEvent
	if err := scanner.Scan(
		&event.ID,
		&event.CampaignID,
		&event.LeadID,
		&event.AccountID,
		&event.AccountEmail,
		&event.Provider,
		&event.Type,
		&event.StepNumber,
		&event.MessageID,
		&event.ThreadID,
		&event.Timestamp,
	); err != nil {
		return emailMessageBackfillEvent{}, err
	}
	return event, nil
}

func backfillInboundEventEmailMessage(cfg BackfillEmailMessagesConfig, event emailMessageBackfillEvent) (bool, error) {
	if event.Provider != AccountProviderGWS {
		return false, nil
	}
	if cfg.GWS == nil {
		return false, fmt.Errorf("gws client is required to backfill %s account %s", event.Provider, event.AccountEmail)
	}

	msg, err := cfg.GWS.GetMessage(event.AccountEmail, event.MessageID)
	if err != nil {
		return false, err
	}
	if msg == nil {
		return false, fmt.Errorf("message %s not found", event.MessageID)
	}
	normalizeBackfilledGWSMessage(msg, event)

	if cfg.DryRun {
		return true, nil
	}

	return true, insertEmailMessage(cfg.DB, emailMessageFromBackfillEvent(event, *msg, EmailMessageDirectionInbound, event.Type))
}

func backfillRelatedSentEmailMessages(cfg BackfillEmailMessagesConfig, inbound emailMessageBackfillEvent) (backfilled int, unsupported int, failed int, err error) {
	events, err := loadRelatedSentEventsMissingEmailMessages(cfg.DB, inbound)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, event := range events {
		if event.Provider != AccountProviderGWS {
			unsupported++
			continue
		}
		if cfg.GWS == nil {
			failed++
			continue
		}

		msg, err := getGWSMessageForBackfillEvent(cfg.GWS, event)
		if err != nil {
			failed++
			slog.Warn("failed to backfill sent email message",
				"event_id", event.ID, "message_id", event.MessageID, "error", err)
			continue
		}
		normalizeBackfilledGWSMessage(msg, event)
		if cfg.DryRun {
			backfilled++
			continue
		}
		if err := insertEmailMessage(cfg.DB, emailMessageFromBackfillEvent(event, *msg, EmailMessageDirectionOutbound, EmailMessageTypeSent)); err != nil {
			failed++
			continue
		}
		backfilled++
	}
	return backfilled, unsupported, failed, nil
}

func backfillProviderThreadEmailMessages(cfg BackfillEmailMessagesConfig, event emailMessageBackfillEvent) (backfilled int, inbound int, sent int, unsupported int, failed int, err error) {
	if strings.TrimSpace(event.ThreadID) == "" {
		return 0, 0, 0, 0, 0, nil
	}
	if event.Provider != AccountProviderGWS {
		return 0, 0, 0, 1, 0, nil
	}
	if cfg.GWS == nil {
		return 0, 0, 0, 0, 1, nil
	}

	messages, err := cfg.GWS.GetThreadMessages(event.AccountEmail, event.ThreadID)
	if err != nil {
		slog.Warn("failed to backfill provider thread messages",
			"event_id", event.ID, "thread_id", event.ThreadID, "error", err)
		return 0, 0, 0, 0, 1, nil
	}

	for _, msg := range messages {
		if strings.TrimSpace(msg.ID) == "" {
			continue
		}
		if msg.ThreadID == "" {
			msg.ThreadID = event.ThreadID
		}
		if msg.Headers == nil {
			msg.Headers = map[string]string{}
		}
		exists, err := emailMessageSnapshotExistsForGWSMessage(cfg.DB, event.CampaignID, event.LeadID, msg)
		if err != nil {
			return backfilled, inbound, sent, unsupported, failed, err
		}
		if exists {
			continue
		}

		direction := EmailMessageDirectionInbound
		messageType := EmailMessageTypeReply
		if sameEmailAddress(msg.From, event.AccountEmail) {
			direction = EmailMessageDirectionOutbound
			messageType = EmailMessageTypeManualReply
		}
		if direction == EmailMessageDirectionInbound && msg.ID == event.MessageID {
			messageType = event.Type
		}

		if cfg.DryRun {
			backfilled++
			if direction == EmailMessageDirectionOutbound {
				sent++
			} else {
				inbound++
			}
			continue
		}

		if err := insertEmailMessage(cfg.DB, emailMessageFromThreadMessage(event, msg, direction, messageType)); err != nil {
			failed++
			continue
		}
		backfilled++
		if direction == EmailMessageDirectionOutbound {
			sent++
		} else {
			inbound++
		}
	}

	return backfilled, inbound, sent, unsupported, failed, nil
}

func loadRelatedSentEventsMissingEmailMessages(db *sql.DB, inbound emailMessageBackfillEvent) ([]emailMessageBackfillEvent, error) {
	query := `
		SELECT
			e.id,
			e.campaign_id,
			e.lead_id,
			e.account_id,
			a.email,
			a.provider,
			e.type,
			e.step_number,
			e.message_id,
			e.thread_id,
			e.timestamp
		FROM events e
		JOIN accounts a ON a.id = e.account_id
		LEFT JOIN email_messages em ON em.message_id = e.message_id AND em.type = e.type
		WHERE e.type = 'sent'
			AND e.campaign_id = ?
			AND e.lead_id = ?
			AND e.message_id <> ''
			AND em.id IS NULL`
	args := []any{inbound.CampaignID, inbound.LeadID}
	if strings.TrimSpace(inbound.ThreadID) != "" {
		query += " AND e.thread_id = ?"
		args = append(args, inbound.ThreadID)
	}
	query += " ORDER BY e.timestamp ASC, e.id ASC LIMIT 20"

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("loading related sent events: %w", err)
	}
	defer rows.Close()

	var events []emailMessageBackfillEvent
	for rows.Next() {
		event, err := scanBackfillEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func getGWSMessageForBackfillEvent(gws GWSClient, event emailMessageBackfillEvent) (*GWSMessage, error) {
	if looksLikeMessageID(event.MessageID) {
		query := "rfc822msgid:" + strings.Trim(event.MessageID, "<>")
		messages, err := gws.ListMessages(event.AccountEmail, query)
		if err == nil && len(messages) > 0 {
			return &messages[0], nil
		}
	}
	return gws.GetMessage(event.AccountEmail, event.MessageID)
}

func normalizeBackfilledGWSMessage(msg *GWSMessage, event emailMessageBackfillEvent) {
	if msg.ID == "" {
		msg.ID = event.MessageID
	}
	if msg.ThreadID == "" {
		msg.ThreadID = event.ThreadID
	}
	if msg.Headers == nil {
		msg.Headers = map[string]string{}
	}
}

func emailMessageFromBackfillEvent(event emailMessageBackfillEvent, msg GWSMessage, direction string, messageType string) EmailMessage {
	textBody := textBodyForInboundSnapshot(msg)
	if textBody == "" {
		textBody = msg.Snippet
	}
	messageID := event.MessageID
	if messageID == "" {
		messageID = msg.ID
	}
	threadID := event.ThreadID
	if threadID == "" {
		threadID = msg.ThreadID
	}
	return EmailMessage{
		CampaignID: event.CampaignID,
		LeadID:     event.LeadID,
		AccountID:  event.AccountID,
		Direction:  direction,
		Type:       messageType,
		StepNumber: event.StepNumber,
		MessageID:  messageID,
		ThreadID:   threadID,
		InReplyTo:  msg.InReplyTo,
		FromEmail:  msg.From,
		ToEmails:   msg.To,
		Subject:    msg.Subject,
		TextBody:   textBody,
		HTMLBody:   msg.HTMLBody,
		Snippet:    msg.Snippet,
		RawHeaders: emailHeadersJSON(msg.Headers),
		OccurredAt: event.Timestamp,
	}
}

func emailMessageFromThreadMessage(event emailMessageBackfillEvent, msg GWSMessage, direction string, messageType string) EmailMessage {
	occurredAt := msg.Date
	if occurredAt.IsZero() {
		occurredAt = event.Timestamp
	}
	textBody := textBodyForInboundSnapshot(msg)
	if textBody == "" {
		textBody = msg.Snippet
	}
	return EmailMessage{
		CampaignID: event.CampaignID,
		LeadID:     event.LeadID,
		AccountID:  event.AccountID,
		Direction:  direction,
		Type:       messageType,
		StepNumber: 0,
		MessageID:  msg.ID,
		ThreadID:   firstNonEmpty(msg.ThreadID, event.ThreadID),
		InReplyTo:  msg.InReplyTo,
		FromEmail:  msg.From,
		ToEmails:   msg.To,
		Subject:    msg.Subject,
		TextBody:   textBody,
		HTMLBody:   msg.HTMLBody,
		Snippet:    msg.Snippet,
		RawHeaders: emailHeadersJSON(msg.Headers),
		OccurredAt: occurredAt,
	}
}

func emailMessageSnapshotExistsForGWSMessage(db *sql.DB, campaignID, leadID int64, msg GWSMessage) (bool, error) {
	candidates := []string{msg.ID}
	if msg.Headers != nil {
		candidates = append(candidates, msg.Headers["Message-ID"], msg.Headers["Message-Id"])
	}
	for _, candidate := range candidates {
		exists, err := emailMessageSnapshotExists(db, campaignID, leadID, candidate)
		if err != nil || exists {
			return exists, err
		}
	}
	return false, nil
}

func sameEmailAddress(value string, email string) bool {
	parsed := parseEmailAddress(value)
	if parsed == "" {
		parsed = strings.TrimSpace(value)
	}
	return strings.EqualFold(parsed, strings.TrimSpace(email))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
