package internal

import (
	"bytes"
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
	CampaignID     int64
	LeadID         int64
	Subject        string
	Body           string
	Now            time.Time
	SecretResolver SecretResolver
	GWS            GWSClient
	SMTPSender     SMTPEmailSender
}

type SendInboxReplyResult struct {
	CampaignID int64  `json:"campaign_id"`
	LeadID     int64  `json:"lead_id"`
	AccountID  int64  `json:"account_id"`
	FromEmail  string `json:"from_email"`
	ToEmail    string `json:"to_email"`
	Subject    string `json:"subject"`
	MessageID  string `json:"message_id"`
	ThreadID   string `json:"thread_id"`
}

func SendInboxReply(cfg SendInboxReplyConfig) (*SendInboxReplyResult, error) {
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
	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	latest, err := latestEmailThreadMessage(cfg.DB, cfg.CampaignID, cfg.LeadID)
	if err != nil {
		return nil, err
	}

	account, err := getAccountByID(cfg.DB, latest.AccountID)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" {
		return nil, fmt.Errorf("account %s is %s", account.Email, account.Status)
	}

	toEmail, err := replyRecipientEmail(cfg.DB, cfg.LeadID, latest)
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
	emailParams := EmailParams{
		FromEmail:  account.Email,
		ToEmail:    toEmail,
		Subject:    subject,
		Body:       body,
		InReplyTo:  inReplyTo,
		References: inReplyTo,
		ThreadID:   latest.ThreadID,
		Date:       now,
	}

	storedMessageID, threadID, err := sendRenderedEmail(TickConfig{
		DB:             cfg.DB,
		GWS:            cfg.GWS,
		SecretResolver: cfg.SecretResolver,
		SMTPSender:     cfg.SMTPSender,
	}, account, emailParams)
	if err != nil {
		return nil, err
	}

	if _, err := execDB(cfg.DB, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id, timestamp)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
		cfg.CampaignID, cfg.LeadID, account.ID, EmailMessageTypeManualReply, storedMessageID, threadID, now); err != nil {
		return nil, fmt.Errorf("inserting manual reply event: %w", err)
	}

	if err := insertEmailMessage(cfg.DB, EmailMessage{
		CampaignID: cfg.CampaignID,
		LeadID:     cfg.LeadID,
		AccountID:  account.ID,
		Direction:  EmailMessageDirectionOutbound,
		Type:       EmailMessageTypeManualReply,
		StepNumber: 0,
		MessageID:  storedMessageID,
		ThreadID:   threadID,
		InReplyTo:  inReplyTo,
		FromEmail:  account.Email,
		ToEmails:   toEmail,
		Subject:    subject,
		TextBody:   body,
		HTMLBody:   plainTextToHTML(body),
		Snippet:    emailSnippetFromBody(body),
		OccurredAt: now,
	}); err != nil {
		return nil, fmt.Errorf("inserting manual reply email message: %w", err)
	}

	return &SendInboxReplyResult{
		CampaignID: cfg.CampaignID,
		LeadID:     cfg.LeadID,
		AccountID:  account.ID,
		FromEmail:  account.Email,
		ToEmail:    toEmail,
		Subject:    subject,
		MessageID:  storedMessageID,
		ThreadID:   threadID,
	}, nil
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
		if address := parseEmailAddress(latest.FromEmail); address != "" {
			return address, nil
		}
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
