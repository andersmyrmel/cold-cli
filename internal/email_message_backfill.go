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

func BackfillEmailMessages(cfg BackfillEmailMessagesConfig) (*BackfillEmailMessagesResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	result := &BackfillEmailMessagesResult{DryRun: cfg.DryRun}

	events, err := loadInboundEventsMissingEmailMessages(cfg.DB, cfg.Since, cfg.Limit)
	if err != nil {
		return nil, err
	}
	result.Scanned = len(events)

	seenSentThreads := map[string]struct{}{}
	for _, event := range events {
		backfilled, err := backfillInboundEventEmailMessage(cfg, event)
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
		threadKey := fmt.Sprintf("%d:%d:%s", event.CampaignID, event.LeadID, event.ThreadID)
		if _, ok := seenSentThreads[threadKey]; ok {
			continue
		}
		seenSentThreads[threadKey] = struct{}{}

		sentBackfilled, sentUnsupported, sentFailed, err := backfillRelatedSentEmailMessages(cfg, event)
		if err != nil {
			return nil, err
		}
		result.Backfilled += sentBackfilled
		result.Sent += sentBackfilled
		result.Unsupported += sentUnsupported
		result.Failed += sentFailed
	}

	return result, nil
}

func loadInboundEventsMissingEmailMessages(db *sql.DB, since time.Time, limit int) ([]emailMessageBackfillEvent, error) {
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
		LEFT JOIN email_messages em ON em.message_id = e.message_id AND em.type = e.type
		WHERE e.type IN ('reply', 'unsubscribe')
			AND e.message_id <> ''
			AND em.id IS NULL`
	args := []any{}
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
