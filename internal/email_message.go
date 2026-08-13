package internal

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

const emailDisplayBodyVersion = "5"

func insertEmailMessage(db *sql.DB, msg EmailMessage) error {
	if msg.OccurredAt.IsZero() {
		msg.OccurredAt = time.Now().UTC()
	}
	msg.OccurredAt = msg.OccurredAt.UTC()

	if strings.TrimSpace(msg.DisplayBody) == "" {
		msg.DisplayBody = emailDisplayBody(msg)
	}
	if strings.TrimSpace(msg.DisplayHTML) == "" {
		msg.DisplayHTML = emailDisplayHTML(msg)
	}

	if strings.TrimSpace(msg.RawHeaders) == "" {
		msg.RawHeaders = "{}"
	}

	_, err := execDB(db, `
		INSERT INTO email_messages (
			campaign_id,
			lead_id,
			account_id,
			direction,
			type,
			step_number,
			scheduled_send_id,
			event_id,
			message_id,
			thread_id,
			in_reply_to,
			from_email,
			to_emails,
			subject,
			text_body,
			display_body,
			display_html,
			html_body,
			snippet,
			raw_headers,
			occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.CampaignID,
		msg.LeadID,
		msg.AccountID,
		msg.Direction,
		msg.Type,
		msg.StepNumber,
		nullableInt64(msg.ScheduledSendID),
		nullableInt64(msg.EventID),
		msg.MessageID,
		msg.ThreadID,
		msg.InReplyTo,
		msg.FromEmail,
		msg.ToEmails,
		msg.Subject,
		msg.TextBody,
		msg.DisplayBody,
		msg.DisplayHTML,
		msg.HTMLBody,
		msg.Snippet,
		msg.RawHeaders,
		msg.OccurredAt,
	)
	return err
}

type ListEmailThreadMessagesOpts struct {
	CampaignID int64
	LeadID     int64
	ThreadID   string
	Limit      int
}

func ListEmailThreadMessages(db *sql.DB, opts ListEmailThreadMessagesOpts) ([]EmailMessage, error) {
	if opts.CampaignID == 0 {
		return nil, errRequired("campaign_id")
	}
	if opts.LeadID == 0 {
		return nil, errRequired("lead_id")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	query := `
		SELECT
			id,
			campaign_id,
			lead_id,
			account_id,
			direction,
			type,
			step_number,
			scheduled_send_id,
			event_id,
			message_id,
			thread_id,
			in_reply_to,
			from_email,
			to_emails,
			subject,
			text_body,
			display_body,
			display_html,
			html_body,
			snippet,
			raw_headers,
			occurred_at,
			created_at
		FROM email_messages
		WHERE campaign_id = ? AND lead_id = ?`
	args := []any{opts.CampaignID, opts.LeadID}

	if strings.TrimSpace(opts.ThreadID) != "" {
		query += " AND thread_id = ?"
		args = append(args, opts.ThreadID)
	}

	query += " ORDER BY occurred_at ASC, id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []EmailMessage
	for rows.Next() {
		msg, err := scanEmailMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func backfillEmailMessageDisplayBodies(db *sql.DB) error {
	currentVersion := emailMessageDisplayBodyVersion(db)
	recomputeAll := currentVersion != emailDisplayBodyVersion

	where := "WHERE (display_body = '' AND (text_body <> '' OR snippet <> '')) OR (display_html = '' AND html_body <> '')"
	if recomputeAll {
		where = "WHERE text_body <> '' OR snippet <> '' OR html_body <> ''"
	}

	rows, err := queryDB(db, `
		SELECT id, direction, type, text_body, snippet, display_body, html_body, display_html
		FROM email_messages
		`+where)
	if err != nil {
		return fmt.Errorf("loading email messages for display body backfill: %w", err)
	}
	defer rows.Close()

	type displayBodyBackfillRow struct {
		ID        int64
		Direction string
		Type      string
		TextBody  string
		Snippet   string
		Current   string
		HTMLBody  string
		HTML      string
	}

	var pending []displayBodyBackfillRow
	for rows.Next() {
		var row displayBodyBackfillRow
		if err := rows.Scan(&row.ID, &row.Direction, &row.Type, &row.TextBody, &row.Snippet, &row.Current, &row.HTMLBody, &row.HTML); err != nil {
			return err
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, row := range pending {
		displayBody := emailDisplayBody(EmailMessage{
			Direction: row.Direction,
			Type:      row.Type,
			TextBody:  row.TextBody,
			Snippet:   row.Snippet,
		})
		displayHTML := emailDisplayHTML(EmailMessage{
			Direction: row.Direction,
			Type:      row.Type,
			HTMLBody:  row.HTMLBody,
		})
		if displayBody == row.Current && displayHTML == row.HTML {
			continue
		}
		if _, err := execDB(db, `UPDATE email_messages SET display_body = ?, display_html = ? WHERE id = ?`, displayBody, displayHTML, row.ID); err != nil {
			return fmt.Errorf("backfilling email message display body %d: %w", row.ID, err)
		}
	}

	if recomputeAll {
		if _, err := execDB(db, `INSERT INTO kv (key, value) VALUES ('email_messages.display_body_version', ?)
			ON CONFLICT(key) DO UPDATE SET value = ?`, emailDisplayBodyVersion, emailDisplayBodyVersion); err != nil {
			return fmt.Errorf("recording email message display body version: %w", err)
		}
	}

	return nil
}

func emailMessageDisplayBodyVersion(db *sql.DB) string {
	var version string
	err := queryRowDB(db, "SELECT value FROM kv WHERE key = 'email_messages.display_body_version'").Scan(&version)
	if err == nil {
		return version
	}
	if err == sql.ErrNoRows {
		return ""
	}
	return ""
}

type SendInboxReplyConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	CampaignID     int64
	LeadID         int64
	Subject        string
	Body           string
	ReplyAll       bool
	IdempotencyKey string
	Now            time.Time
	SecretResolver SecretResolver
	GWS            GWSClient
	IMAP           IMAPMessageLister
	SMTPSender     SMTPEmailSender
	SentAppender   IMAPSentAppender
	RefreshThread  bool
}

type SendInboxReplyResult struct {
	CampaignID     int64    `json:"campaign_id"`
	LeadID         int64    `json:"lead_id"`
	AccountID      int64    `json:"account_id"`
	FromName       string   `json:"from_name,omitempty"`
	FromEmail      string   `json:"from_email"`
	ToEmail        string   `json:"to_email"`
	CcEmails       []string `json:"cc_emails,omitempty"`
	Subject        string   `json:"subject"`
	MessageID      string   `json:"message_id"`
	ThreadID       string   `json:"thread_id"`
	SentMailbox    string   `json:"sent_mailbox,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
	AlreadySent    bool     `json:"already_sent"`
	Warnings       []string `json:"warnings,omitempty"`
}

type PreviewInboxReplyConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	CampaignID     int64
	LeadID         int64
	Subject        string
	Body           string
	ReplyAll       bool
	IdempotencyKey string
}

type InboxReplyPreview struct {
	CampaignID      int64    `json:"campaign_id"`
	LeadID          int64    `json:"lead_id"`
	AccountID       int64    `json:"account_id"`
	FromName        string   `json:"from_name,omitempty"`
	FromEmail       string   `json:"from_email"`
	ToEmail         string   `json:"to_email"`
	CcEmails        []string `json:"cc_emails,omitempty"`
	Subject         string   `json:"subject"`
	Body            string   `json:"body"`
	InReplyTo       string   `json:"in_reply_to"`
	References      string   `json:"references"`
	ThreadID        string   `json:"thread_id"`
	LatestDirection string   `json:"latest_direction"`
	LatestType      string   `json:"latest_type"`
	IdempotencyKey  string   `json:"idempotency_key"`
	Warnings        []string `json:"warnings,omitempty"`
}

func PreviewInboxReply(cfg PreviewInboxReplyConfig) (*InboxReplyPreview, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	if cfg.CampaignID == 0 {
		return nil, errRequired("campaign_id")
	}
	if cfg.LeadID == 0 {
		return nil, errRequired("lead_id")
	}
	body := strings.TrimSpace(cfg.Body)
	if body == "" {
		return nil, errRequired("body")
	}

	var campaignWorkspace, sequenceFile, sequenceContent string
	if err := queryRowDB(cfg.DB, `SELECT workspace_id, sequence_file, sequence_content FROM campaigns WHERE id = ?`, cfg.CampaignID).
		Scan(&campaignWorkspace, &sequenceFile, &sequenceContent); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("campaign %d not found", cfg.CampaignID)
		}
		return nil, fmt.Errorf("loading campaign: %w", err)
	}
	if workspace := strings.TrimSpace(cfg.WorkspaceID); workspace != "" && campaignWorkspace != NormalizeWorkspaceID(workspace) {
		return nil, fmt.Errorf("campaign %d is not in workspace %s", cfg.CampaignID, NormalizeWorkspaceID(workspace))
	}

	var globalStatus string
	if err := queryRowDB(cfg.DB, `SELECT global_status FROM leads WHERE id = ?`, cfg.LeadID).Scan(&globalStatus); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("lead %d not found", cfg.LeadID)
		}
		return nil, fmt.Errorf("loading lead status: %w", err)
	}
	if globalStatus != "active" {
		return nil, fmt.Errorf("lead %d is %s; manual reply blocked", cfg.LeadID, globalStatus)
	}
	var campaignLeadExists int
	if err := queryRowDB(cfg.DB, `SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?`, cfg.CampaignID, cfg.LeadID).
		Scan(&campaignLeadExists); err != nil {
		return nil, fmt.Errorf("checking campaign lead: %w", err)
	}
	if campaignLeadExists == 0 {
		return nil, fmt.Errorf("lead %d is not in campaign %d", cfg.LeadID, cfg.CampaignID)
	}

	var suppressedType string
	err := queryRowDB(cfg.DB, `SELECT type FROM events
		WHERE campaign_id = ? AND lead_id = ? AND type IN ('unsubscribe', 'bounce')
		ORDER BY timestamp DESC, id DESC LIMIT 1`, cfg.CampaignID, cfg.LeadID).Scan(&suppressedType)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("checking reply suppression: %w", err)
	}
	if suppressedType != "" {
		return nil, fmt.Errorf("lead has a recorded %s event; manual reply blocked", suppressedType)
	}

	latest, err := latestEmailThreadMessage(cfg.DB, cfg.CampaignID, cfg.LeadID)
	if err != nil {
		return nil, err
	}
	switch latest.Type {
	case EmailMessageTypeBounce, EmailMessageTypeUnsubscribe, EmailMessageTypeAutoReply:
		return nil, fmt.Errorf("latest thread message is %s; manual reply blocked", latest.Type)
	}

	account, err := getAccountByID(cfg.DB, latest.AccountID)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" {
		return nil, fmt.Errorf("account %s is %s", account.Email, account.Status)
	}
	if account.WorkspaceID != campaignWorkspace {
		return nil, fmt.Errorf("thread account %s is not in campaign workspace", account.Email)
	}

	toEmail, err := replyRecipientEmail(cfg.DB, cfg.LeadID, latest)
	if err != nil {
		return nil, err
	}
	ccEmails, err := replyCCEmails(latest, account.Email, toEmail, cfg.ReplyAll)
	if err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(cfg.Subject)
	if subject == "" {
		subject = replySubject(latest.Subject)
	}
	if subject == "" {
		return nil, errRequired("subject")
	}

	inReplyTo := replyMessageID(latest)
	if !looksLikeMessageID(inReplyTo) {
		return nil, fmt.Errorf("latest thread message has no usable RFC Message-ID")
	}
	references := replyReferences(latest, inReplyTo)
	fromName := replyFromName(sequenceFile, sequenceContent)

	params := EmailParams{
		FromName: fromName, FromEmail: account.Email, ToEmail: toEmail, CcEmails: ccEmails,
		Subject: subject, Body: body, InReplyTo: inReplyTo, References: references,
		ThreadID: latest.ThreadID,
	}
	if err := ValidateEmailParamsHeaders(params); err != nil {
		return nil, err
	}

	warnings := []string{}
	if !cfg.ReplyAll && strings.TrimSpace(latest.CcEmails) != "" {
		warnings = append(warnings, "latest message has Cc recipients; use --reply-all to include them")
	}
	idempotencyKey := manualReplyIdempotencyKey(cfg.IdempotencyKey, cfg.CampaignID, cfg.LeadID, account.ID, toEmail, ccEmails, subject, body)

	return &InboxReplyPreview{
		CampaignID: cfg.CampaignID, LeadID: cfg.LeadID, AccountID: account.ID,
		FromName: fromName, FromEmail: account.Email, ToEmail: toEmail, CcEmails: ccEmails,
		Subject: subject, Body: body, InReplyTo: inReplyTo, References: references,
		ThreadID: latest.ThreadID, LatestDirection: latest.Direction, LatestType: latest.Type,
		IdempotencyKey: idempotencyKey, Warnings: warnings,
	}, nil
}

func SendInboxReply(cfg SendInboxReplyConfig) (*SendInboxReplyResult, error) {
	if cfg.RefreshThread {
		if _, err := SyncEmailThread(SyncEmailThreadConfig{
			DB: cfg.DB, WorkspaceID: cfg.WorkspaceID, CampaignID: cfg.CampaignID, LeadID: cfg.LeadID,
			SecretResolver: cfg.SecretResolver, GWS: cfg.GWS, IMAP: cfg.IMAP,
		}); err != nil {
			return nil, fmt.Errorf("refreshing thread before reply: %w", err)
		}
	}
	preview, err := PreviewInboxReply(PreviewInboxReplyConfig{
		DB: cfg.DB, WorkspaceID: cfg.WorkspaceID, CampaignID: cfg.CampaignID, LeadID: cfg.LeadID,
		Subject: cfg.Subject, Body: cfg.Body, ReplyAll: cfg.ReplyAll, IdempotencyKey: cfg.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	account, err := getAccountByID(cfg.DB, preview.AccountID)
	if err != nil {
		return nil, err
	}
	emailParams := EmailParams{
		FromName: preview.FromName, FromEmail: account.Email, ToEmail: preview.ToEmail, CcEmails: preview.CcEmails,
		Subject: preview.Subject, Body: preview.Body, InReplyTo: preview.InReplyTo,
		References: preview.References, ThreadID: preview.ThreadID, Date: now,
		MessageID: GenerateRFCMessageID(account.Email),
	}

	existing, reserved, err := reserveManualReplyAttempt(cfg.DB, preview, emailParams.MessageID, now)
	if err != nil {
		return nil, err
	}
	if !reserved {
		return existing, nil
	}

	storedMessageID, threadID, err := sendRenderedEmail(TickConfig{
		DB:             cfg.DB,
		GWS:            cfg.GWS,
		SecretResolver: cfg.SecretResolver,
		SMTPSender:     cfg.SMTPSender,
	}, account, emailParams)
	if err != nil {
		markManualReplyAttemptFailed(cfg.DB, preview.IdempotencyKey, err)
		return nil, err
	}
	emailParams.MessageID = storedMessageID
	emailParams.ThreadID = threadID

	sentMailbox := ""
	warnings := append([]string{}, preview.Warnings...)
	if account.Provider == AccountProviderSMTPIMAP {
		appender := cfg.SentAppender
		if appender == nil {
			appender = NewIMAPTransport(cfg.SecretResolver)
		}
		mailbox, appendErr := appender.AppendSent(account, emailParams)
		if appendErr != nil {
			warnings = append(warnings, "email delivered, but the Migadu Sent copy failed: "+appendErr.Error())
		} else {
			sentMailbox = mailbox
		}
	}

	if err := persistManualReplyDelivery(cfg.DB, preview, storedMessageID, threadID, sentMailbox, warnings, now); err != nil {
		return nil, fmt.Errorf("email was accepted by the provider, but recording it failed; do not retry until message %s is reconciled: %w", storedMessageID, err)
	}

	return &SendInboxReplyResult{
		CampaignID: cfg.CampaignID, LeadID: cfg.LeadID, AccountID: account.ID,
		FromName: preview.FromName, FromEmail: account.Email, ToEmail: preview.ToEmail, CcEmails: preview.CcEmails,
		Subject: preview.Subject, MessageID: storedMessageID, ThreadID: threadID,
		SentMailbox: sentMailbox, IdempotencyKey: preview.IdempotencyKey, Warnings: warnings,
	}, nil
}

type manualReplyAttempt struct {
	Status         string
	CampaignID     int64
	LeadID         int64
	AccountID      int64
	FromEmail      string
	ToEmail        string
	CcEmails       string
	Subject        string
	MessageID      string
	ThreadID       string
	SentMailbox    string
	WarningMessage string
	ErrorMessage   string
}

func reserveManualReplyAttempt(db *sql.DB, preview *InboxReplyPreview, messageID string, now time.Time) (*SendInboxReplyResult, bool, error) {
	_, err := execDB(db, `INSERT INTO manual_reply_attempts (
		campaign_id, lead_id, account_id, idempotency_key, status,
		from_email, to_email, cc_emails, subject, message_id, thread_id, created_at
	) VALUES (?, ?, ?, ?, 'sending', ?, ?, ?, ?, ?, ?, ?)`,
		preview.CampaignID, preview.LeadID, preview.AccountID, preview.IdempotencyKey,
		preview.FromEmail, preview.ToEmail, strings.Join(preview.CcEmails, ", "),
		preview.Subject, messageID, preview.ThreadID, now)
	if err == nil {
		return nil, true, nil
	}
	if !isUniqueConstraintError(err) {
		return nil, false, fmt.Errorf("reserving manual reply: %w", err)
	}

	attempt, loadErr := loadManualReplyAttempt(db, preview.IdempotencyKey)
	if loadErr != nil {
		return nil, false, loadErr
	}
	switch attempt.Status {
	case "sent":
		warnings := decodeWarnings(attempt.WarningMessage)
		return &SendInboxReplyResult{
			CampaignID: attempt.CampaignID, LeadID: attempt.LeadID, AccountID: attempt.AccountID,
			FromEmail: attempt.FromEmail, ToEmail: attempt.ToEmail, CcEmails: splitStoredAddresses(attempt.CcEmails),
			Subject: attempt.Subject, MessageID: attempt.MessageID, ThreadID: attempt.ThreadID,
			SentMailbox: attempt.SentMailbox, IdempotencyKey: preview.IdempotencyKey,
			AlreadySent: true, Warnings: warnings,
		}, false, nil
	case "sending":
		return nil, false, fmt.Errorf("an identical reply is already sending or has an unknown outcome; do not retry until message %s is reconciled", attempt.MessageID)
	case "failed":
		return nil, false, fmt.Errorf("an identical reply attempt previously failed (%s); inspect it before choosing a new idempotency key", attempt.ErrorMessage)
	default:
		return nil, false, fmt.Errorf("manual reply attempt has unsupported status %q", attempt.Status)
	}
}

func loadManualReplyAttempt(db *sql.DB, idempotencyKey string) (manualReplyAttempt, error) {
	var attempt manualReplyAttempt
	err := queryRowDB(db, `SELECT status, campaign_id, lead_id, account_id, from_email, to_email,
		cc_emails, subject, message_id, thread_id, sent_mailbox, warning_message, error_message
		FROM manual_reply_attempts WHERE idempotency_key = ?`, idempotencyKey).Scan(
		&attempt.Status, &attempt.CampaignID, &attempt.LeadID, &attempt.AccountID,
		&attempt.FromEmail, &attempt.ToEmail, &attempt.CcEmails, &attempt.Subject,
		&attempt.MessageID, &attempt.ThreadID, &attempt.SentMailbox,
		&attempt.WarningMessage, &attempt.ErrorMessage,
	)
	if err != nil {
		return manualReplyAttempt{}, fmt.Errorf("loading manual reply attempt: %w", err)
	}
	return attempt, nil
}

func markManualReplyAttemptFailed(db *sql.DB, idempotencyKey string, sendErr error) {
	_, _ = execDB(db, `UPDATE manual_reply_attempts SET status = 'failed', error_message = ? WHERE idempotency_key = ?`,
		sendErr.Error(), idempotencyKey)
}

func persistManualReplyDelivery(db *sql.DB, preview *InboxReplyPreview, messageID, threadID, sentMailbox string, warnings []string, now time.Time) error {
	warningsJSON, _ := json.Marshal(warnings)
	metadataJSON, _ := json.Marshal(map[string]any{
		"idempotency_key": preview.IdempotencyKey,
		"sent_mailbox":    sentMailbox,
		"warnings":        warnings,
	})
	rawHeaders := map[string]string{
		"Message-ID":  messageID,
		"In-Reply-To": preview.InReplyTo,
		"References":  preview.References,
	}
	if len(preview.CcEmails) > 0 {
		rawHeaders["Cc"] = strings.Join(preview.CcEmails, ", ")
	}
	rawHeadersJSON, _ := json.Marshal(rawHeaders)

	msg := EmailMessage{
		CampaignID: preview.CampaignID, LeadID: preview.LeadID, AccountID: preview.AccountID,
		Direction: EmailMessageDirectionOutbound, Type: EmailMessageTypeManualReply,
		MessageID: messageID, ThreadID: threadID, InReplyTo: preview.InReplyTo,
		FromEmail: preview.FromEmail, ToEmails: preview.ToEmail, Subject: preview.Subject,
		TextBody: preview.Body, HTMLBody: plainTextToHTML(preview.Body),
		Snippet: emailSnippetFromBody(preview.Body), RawHeaders: string(rawHeadersJSON), OccurredAt: now,
	}
	msg.DisplayBody = emailDisplayBody(msg)
	msg.DisplayHTML = emailDisplayHTML(msg)

	tx, err := beginTx(db)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO events (
		campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp, metadata
	) VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		preview.CampaignID, preview.LeadID, preview.AccountID, EmailMessageTypeManualReply,
		messageID, threadID, now.Format(time.RFC3339), string(metadataJSON)); err != nil {
		return fmt.Errorf("inserting manual reply event: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO email_messages (
		campaign_id, lead_id, account_id, direction, type, step_number,
		message_id, thread_id, in_reply_to, from_email, to_emails, subject,
		text_body, display_body, display_html, html_body, snippet, raw_headers, occurred_at
	) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.CampaignID, msg.LeadID, msg.AccountID, msg.Direction, msg.Type,
		msg.MessageID, msg.ThreadID, msg.InReplyTo, msg.FromEmail, msg.ToEmails,
		msg.Subject, msg.TextBody, msg.DisplayBody, msg.DisplayHTML, msg.HTMLBody,
		msg.Snippet, msg.RawHeaders, msg.OccurredAt); err != nil {
		return fmt.Errorf("inserting manual reply email message: %w", err)
	}
	result, err := tx.Exec(`UPDATE manual_reply_attempts SET status = 'sent', message_id = ?, thread_id = ?,
		sent_mailbox = ?, warning_message = ?, error_message = '', sent_at = ?
		WHERE idempotency_key = ? AND status = 'sending'`,
		messageID, threadID, sentMailbox, string(warningsJSON), now, preview.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("finalizing manual reply attempt: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("manual reply attempt was not in sending state")
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_send_at = ? WHERE id = ?`, now, preview.AccountID); err != nil {
		return fmt.Errorf("updating sender last-send time: %w", err)
	}
	return tx.Commit()
}

func manualReplyIdempotencyKey(userKey string, campaignID, leadID, accountID int64, toEmail string, ccEmails []string, subject, body string) string {
	material := strings.TrimSpace(userKey)
	if material == "" {
		material = fmt.Sprintf("v1\n%d\n%d\n%d\n%s\n%s\n%s\n%s",
			campaignID, leadID, accountID, strings.ToLower(toEmail),
			strings.ToLower(strings.Join(ccEmails, ",")), subject, body)
	} else {
		material = fmt.Sprintf("user-v1\n%d\n%d\n%s", campaignID, leadID, material)
	}
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("manual-reply:v1:%x", sum[:])
}

func decodeWarnings(value string) []string {
	var warnings []string
	if strings.TrimSpace(value) != "" {
		_ = json.Unmarshal([]byte(value), &warnings)
	}
	return warnings
}

func splitStoredAddresses(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func latestEmailThreadMessage(db *sql.DB, campaignID, leadID int64) (EmailMessage, error) {
	msg, err := scanEmailMessage(queryRowDB(db, `
		SELECT
			id,
			campaign_id,
			lead_id,
			account_id,
			direction,
			type,
			step_number,
			scheduled_send_id,
			event_id,
			message_id,
			thread_id,
			in_reply_to,
			from_email,
			to_emails,
			subject,
			text_body,
			display_body,
			display_html,
			html_body,
			snippet,
			raw_headers,
			occurred_at,
			created_at
		FROM email_messages
		WHERE campaign_id = ? AND lead_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT 1`, campaignID, leadID))
	if err == sql.ErrNoRows {
		return EmailMessage{}, fmt.Errorf("no stored email thread for campaign_id=%d lead_id=%d", campaignID, leadID)
	}
	if err != nil {
		return EmailMessage{}, err
	}
	return msg, nil
}

func getAccountByID(db *sql.DB, accountID int64) (Account, error) {
	account, err := scanAccount(queryRowDB(db, "SELECT "+accountSelectColumns()+" FROM accounts WHERE id = ?", accountID))
	if err == sql.ErrNoRows {
		return Account{}, fmt.Errorf("account %d not found", accountID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("loading account %d: %w", accountID, err)
	}
	return account, nil
}

func replyRecipientEmail(db *sql.DB, leadID int64, latest EmailMessage) (string, error) {
	if latest.Direction == EmailMessageDirectionInbound {
		if strings.TrimSpace(latest.ReplyToEmails) != "" {
			addresses, err := mail.ParseAddressList(latest.ReplyToEmails)
			if err != nil {
				return "", fmt.Errorf("parsing latest Reply-To header: %w", err)
			}
			if len(addresses) != 1 {
				return "", fmt.Errorf("latest Reply-To contains %d addresses; one recipient is required", len(addresses))
			}
			return addresses[0].Address, nil
		}
		if address := parseEmailAddress(latest.FromEmail); address != "" {
			return address, nil
		}
	} else if strings.TrimSpace(latest.ToEmails) != "" {
		addresses, err := mail.ParseAddressList(latest.ToEmails)
		if err != nil {
			return "", fmt.Errorf("parsing latest To header: %w", err)
		}
		if len(addresses) != 1 {
			return "", fmt.Errorf("latest To header contains %d addresses; one primary recipient is required", len(addresses))
		}
		return addresses[0].Address, nil
	}

	var leadEmail string
	if err := queryRowDB(db, "SELECT email FROM leads WHERE id = ?", leadID).Scan(&leadEmail); err != nil {
		return "", fmt.Errorf("loading lead email: %w", err)
	}
	leadEmail = strings.TrimSpace(leadEmail)
	if leadEmail == "" {
		return "", fmt.Errorf("lead %d has no email", leadID)
	}
	return leadEmail, nil
}

func replyCCEmails(latest EmailMessage, accountEmail, toEmail string, replyAll bool) ([]string, error) {
	if !replyAll {
		return nil, nil
	}
	values := []string{latest.ToEmails, latest.CcEmails}
	seen := map[string]struct{}{
		strings.ToLower(strings.TrimSpace(accountEmail)): {},
		strings.ToLower(strings.TrimSpace(toEmail)):      {},
	}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		addresses, err := mail.ParseAddressList(value)
		if err != nil {
			return nil, fmt.Errorf("parsing reply-all recipients: %w", err)
		}
		for _, address := range addresses {
			email := strings.TrimSpace(address.Address)
			key := strings.ToLower(email)
			if email == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, email)
		}
	}
	return result, nil
}

func replyFromName(sequenceFile, sequenceContent string) string {
	var seq *Sequence
	var err error
	if strings.TrimSpace(sequenceContent) != "" {
		seq, err = ParseSequenceFromBytes([]byte(sequenceContent))
	} else if strings.TrimSpace(sequenceFile) != "" {
		seq, err = ParseSequence(sequenceFile)
	}
	if err != nil || seq == nil {
		return ""
	}
	return strings.TrimSpace(seq.Defaults.FromName)
}

func replyReferences(latest EmailMessage, inReplyTo string) string {
	var values []string
	headers := map[string]string{}
	if err := json.Unmarshal([]byte(latest.RawHeaders), &headers); err == nil {
		values = append(values, messageIDs(firstEmailHeader(headers, "References"))...)
	}
	values = append(values, messageIDs(latest.InReplyTo)...)
	if looksLikeMessageID(latest.ThreadID) {
		values = append(values, latest.ThreadID)
	}
	values = append(values, inReplyTo)

	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !looksLikeMessageID(value) {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return strings.Join(result, " ")
}

func messageIDs(value string) []string {
	var result []string
	for {
		start := strings.Index(value, "<")
		if start < 0 {
			break
		}
		end := strings.Index(value[start:], ">")
		if end < 0 {
			break
		}
		candidate := value[start : start+end+1]
		if looksLikeMessageID(candidate) {
			result = append(result, candidate)
		}
		value = value[start+end+1:]
	}
	return result
}

func parseEmailAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	address, err := mail.ParseAddress(value)
	if err == nil {
		return address.Address
	}
	if strings.Contains(value, "@") && !strings.Contains(value, " ") {
		return strings.Trim(value, "<>")
	}
	return ""
}

func replySubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}

func replyMessageID(msg EmailMessage) string {
	if looksLikeMessageID(msg.MessageID) {
		return msg.MessageID
	}

	headers := map[string]string{}
	if err := json.Unmarshal([]byte(msg.RawHeaders), &headers); err == nil {
		if id := strings.TrimSpace(headers["Message-ID"]); id != "" {
			return id
		}
		if id := strings.TrimSpace(headers["Message-Id"]); id != "" {
			return id
		}
	}
	return msg.MessageID
}

func looksLikeMessageID(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "<") && strings.Contains(value, "@") && strings.HasSuffix(value, ">")
}

type emailMessageScanner interface {
	Scan(dest ...any) error
}

func scanEmailMessage(scanner emailMessageScanner) (EmailMessage, error) {
	var msg EmailMessage
	var scheduledSendID sql.NullInt64
	var eventID sql.NullInt64
	if err := scanner.Scan(
		&msg.ID,
		&msg.CampaignID,
		&msg.LeadID,
		&msg.AccountID,
		&msg.Direction,
		&msg.Type,
		&msg.StepNumber,
		&scheduledSendID,
		&eventID,
		&msg.MessageID,
		&msg.ThreadID,
		&msg.InReplyTo,
		&msg.FromEmail,
		&msg.ToEmails,
		&msg.Subject,
		&msg.TextBody,
		&msg.DisplayBody,
		&msg.DisplayHTML,
		&msg.HTMLBody,
		&msg.Snippet,
		&msg.RawHeaders,
		&msg.OccurredAt,
		&msg.CreatedAt,
	); err != nil {
		return EmailMessage{}, err
	}
	if scheduledSendID.Valid {
		msg.ScheduledSendID = &scheduledSendID.Int64
	}
	if eventID.Valid {
		msg.EventID = &eventID.Int64
	}
	hydrateEmailMessageHeaderFields(&msg)
	return msg, nil
}

func hydrateEmailMessageHeaderFields(msg *EmailMessage) {
	if msg == nil {
		return
	}

	headers := map[string]string{}
	if err := json.Unmarshal([]byte(msg.RawHeaders), &headers); err != nil {
		return
	}

	if strings.TrimSpace(msg.CcEmails) == "" {
		msg.CcEmails = firstEmailHeader(headers, "Cc")
	}
	if strings.TrimSpace(msg.BccEmails) == "" {
		msg.BccEmails = firstEmailHeader(headers, "Bcc")
	}
	if strings.TrimSpace(msg.ReplyToEmails) == "" {
		msg.ReplyToEmails = firstEmailHeader(headers, "Reply-To")
	}
}

func firstEmailHeader(headers map[string]string, names ...string) string {
	if len(headers) == 0 {
		return ""
	}

	for _, name := range names {
		for key, value := range headers {
			if strings.EqualFold(key, name) && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}

	return ""
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func errRequired(field string) error {
	return fmt.Errorf("%s is required", field)
}

func emailSnippetFromBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) <= 240 {
		return body
	}
	return body[:240]
}

func emailHeadersJSON(headers map[string]string) string {
	if len(headers) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(headers); err != nil {
		return "{}"
	}
	return strings.TrimSpace(buf.String())
}

func textBodyForInboundSnapshot(msg GWSMessage) string {
	if strings.TrimSpace(msg.TextBody) != "" {
		return msg.TextBody
	}
	return msg.Snippet
}
