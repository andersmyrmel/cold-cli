package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"
)

// TickResult holds the summary of a tick invocation.
type TickResult struct {
	RepliesDetected      int  `json:"replies_detected"`
	UnsubscribesDetected int  `json:"unsubscribes_detected"`
	BouncesDetected      int  `json:"bounces_detected"`
	Sent                 int  `json:"sent"`
	Failed               int  `json:"failed"`
	Skipped              int  `json:"skipped"`
	DryRun               bool `json:"dry_run"`
}

// TickConfig holds configuration for a tick invocation.
type TickConfig struct {
	DB                 *sql.DB
	GWS                GWSClient
	DryRun             bool
	SendNow            bool           // ignore send_at timestamps, send all pending
	Now                time.Time      // injectable for testing
	NoSleep            bool           // skip inter-send sleep (for testing)
	Timezone           *time.Location // for daily limit day boundary; defaults to UTC
	UnsubscribeHeader  bool           // add List-Unsubscribe header (off by default for cold email)
	UnsubscribeSubject string         // subject for List-Unsubscribe mailto header
}

// Tick runs one tick cycle: poll replies, poll bounces, send due emails.
func Tick(cfg TickConfig) (*TickResult, error) {
	result := &TickResult{DryRun: cfg.DryRun}

	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}

	tz := cfg.Timezone
	if tz == nil {
		tz = time.UTC
	}

	// Load active accounts
	accounts, err := loadActiveAccounts(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("loading accounts: %w", err)
	}

	if len(accounts) == 0 {
		return result, nil
	}

	// Early exit: skip everything if no active campaigns
	var activeCampaigns int
	queryRowDB(cfg.DB, "SELECT COUNT(*) FROM campaigns WHERE status = 'active'").Scan(&activeCampaigns)
	if activeCampaigns == 0 {
		return result, nil
	}

	// 1. Poll for replies and unsubscribes
	replies, unsubs, err := ProcessReplies(cfg.DB, cfg.GWS, accounts)
	if err != nil {
		slog.Warn("reply detection error", "error", err)
	}
	result.RepliesDetected = replies
	result.UnsubscribesDetected = unsubs

	// 2. Poll for bounces
	bounces, err := ProcessBounces(cfg.DB, cfg.GWS, accounts)
	if err != nil {
		slog.Warn("bounce detection error", "error", err)
	}
	result.BouncesDetected = bounces

	// Update last_poll_at so next tick only checks new messages
	SetLastPollAt(cfg.DB, now)

	// 3. Preload daily send counts per account (timezone-aware day boundary)
	dailyCounts, err := preloadDailyCounts(cfg.DB, now, tz)
	if err != nil {
		return nil, fmt.Errorf("loading daily counts: %w", err)
	}

	// Build account lookup
	accountMap := map[int64]Account{}
	dailyLimits := map[int64]int{}
	var accountIDs []int64
	for _, a := range accounts {
		accountMap[a.ID] = a
		dailyLimits[a.ID] = a.DailyLimit
		accountIDs = append(accountIDs, a.ID)
	}

	if err := RebalancePendingSchedules(cfg.DB, accountIDs); err != nil {
		return nil, fmt.Errorf("rebalancing pending schedules: %w", err)
	}

	// 4. Find due sends from active campaigns
	var dueSends []dueSend
	if cfg.SendNow {
		dueSends, err = loadAllPendingSends(cfg.DB)
	} else {
		dueSends, err = loadDueSends(cfg.DB, now)
	}
	if err != nil {
		return nil, fmt.Errorf("loading due sends: %w", err)
	}

	if len(dueSends) == 0 {
		return result, nil
	}

	// 5. Load sequences for active campaigns (needed for rendering)
	seqCache := map[int64]*Sequence{}

	// 6. Send loop
	for i, send := range dueSends {
		currentSend, currentSendAt, ok, err := refreshPendingSend(cfg.DB, send)
		if err != nil {
			return nil, fmt.Errorf("refreshing pending send %d: %w", send.ID, err)
		}
		if !ok {
			continue
		}
		if currentSendAt.After(now) && !cfg.SendNow {
			continue
		}
		send = currentSend

		account, ok := accountMap[send.AccountID]
		if !ok {
			slog.Warn("skipping send for account without an enabled sender transport",
				"send_id", send.ID, "account_id", send.AccountID)
			result.Skipped++
			continue
		}

		// Check daily limit
		if dailyCounts[send.AccountID] >= dailyLimits[send.AccountID] {
			if err := RebalancePendingSchedules(cfg.DB, []int64{send.AccountID}); err != nil {
				return nil, fmt.Errorf("rebalancing account %d after daily limit hit: %w", send.AccountID, err)
			}
			result.Skipped++
			continue
		}

		// Check send window and send day — if outside, leave as pending for next tick
		if !isInSendWindow(cfg.DB, send.CampaignID, send.LeadID, now) {
			continue
		}

		// Load sequence if not cached
		seq, ok := seqCache[send.CampaignID]
		if !ok {
			seq, err = loadSequenceForCampaign(cfg.DB, send.CampaignID)
			if err != nil {
				slog.Error("failed to load sequence",
					"campaign_id", send.CampaignID, "error", err)
				result.Failed++
				continue
			}
			seqCache[send.CampaignID] = seq
		}

		// Load lead fields
		leadFields, err := loadLeadFields(cfg.DB, send.LeadID)
		if err != nil {
			slog.Error("failed to load lead",
				"lead_id", send.LeadID, "error", err)
			result.Failed++
			continue
		}

		// Build email
		emailParams := BuildEmailForSend(seq, send.StepNumber, send.VariantIndex, leadFields, account.Email)
		if len(emailParams.StrippedVars) > 0 {
			slog.Warn("stripped unresolved template variables",
				"send_id", send.ID, "lead", leadFields["email"],
				"step", send.StepNumber, "stripped", emailParams.StrippedVars)
		}
		if cfg.UnsubscribeHeader {
			emailParams.UnsubscribeEmail = account.Email
			emailParams.UnsubscribeSubject = cfg.UnsubscribeSubject
			if emailParams.UnsubscribeSubject == "" {
				emailParams.UnsubscribeSubject = "Unsubscribe"
			}
		}
		if emailParams.ToEmail == "" {
			errMsg := "could not build email: missing recipient"
			slog.Error(errMsg, "send_id", send.ID, "step", send.StepNumber)
			markSendFailed(cfg.DB, send, errMsg)
			result.Failed++
			continue
		}

		// For follow-ups, add threading headers and derive the visible subject
		// from the rendered step-1 subject before validating emptiness.
		if send.StepNumber > 1 && send.ParentMessageID != "" {
			originalEmail := BuildEmailForSend(seq, 1, send.VariantIndex, leadFields, account.Email)
			PrepareFollowUp(&emailParams, send.ParentMessageID, send.ThreadID, originalEmail.Subject)
		}

		if strings.TrimSpace(emailParams.Subject) == "" {
			errMsg := "empty subject after rendering"
			slog.Error("send aborted: "+errMsg,
				"send_id", send.ID, "lead", emailParams.ToEmail, "step", send.StepNumber)
			markSendFailed(cfg.DB, send, errMsg)
			result.Failed++
			continue
		}
		if strings.TrimSpace(emailParams.Body) == "" {
			errMsg := "empty body after rendering"
			slog.Error("send aborted: "+errMsg,
				"send_id", send.ID, "lead", emailParams.ToEmail, "step", send.StepNumber)
			markSendFailed(cfg.DB, send, errMsg)
			result.Failed++
			continue
		}

		if cfg.DryRun {
			fmt.Printf("[dry-run] would send step %d to %s via %s\n",
				send.StepNumber, emailParams.ToEmail, account.Email)
			result.Sent++
			continue
		}

		// Build raw message
		rawMsg := BuildRawMessage(emailParams)

		// Send via gws
		gmailMsgID, threadID, err := cfg.GWS.SendEmail(account.Email, emailParams.ToEmail, rawMsg, emailParams.ThreadID)
		if err != nil {
			errMsg := err.Error()
			slog.Error("send failed",
				"send_id", send.ID, "step", send.StepNumber,
				"to", emailParams.ToEmail, "account", account.Email,
				"error", err)
			markSendFailed(cfg.DB, send, errMsg)
			result.Failed++
			continue
		}

		// Validate response
		if gmailMsgID == "" || threadID == "" {
			errMsg := "gws returned empty message_id or thread_id"
			slog.Error(errMsg, "send_id", send.ID)
			markSendFailed(cfg.DB, send, errMsg)
			result.Failed++
			continue
		}

		storedMessageID := gmailMsgID
		sentMessage, err := cfg.GWS.GetMessage(account.Email, gmailMsgID)
		if err != nil {
			slog.Warn("failed to fetch sent message headers; falling back to Gmail API id",
				"send_id", send.ID, "gmail_message_id", gmailMsgID, "error", err)
		} else if rfcMessageID := extractRFCMessageID(sentMessage); rfcMessageID != "" {
			storedMessageID = rfcMessageID
		} else {
			slog.Warn("sent message missing Message-ID header; falling back to Gmail API id",
				"send_id", send.ID, "gmail_message_id", gmailMsgID)
		}

		// Mark sent
		if _, err := execDB(cfg.DB, `UPDATE scheduled_sends SET status = 'sent', message_id = ?, sent_at = ?
			WHERE id = ?`, storedMessageID, now.UTC().Format(time.RFC3339), send.ID); err != nil {
			slog.Error("email sent but failed to mark as sent in DB",
				"send_id", send.ID, "message_id", storedMessageID, "error", err)
		}

		// Insert event
		if _, err := execDB(cfg.DB, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
			VALUES (?, ?, ?, 'sent', ?, ?, ?)`,
			send.CampaignID, send.LeadID, send.AccountID, send.StepNumber, storedMessageID, threadID); err != nil {
			slog.Error("failed to insert sent event",
				"send_id", send.ID, "message_id", storedMessageID, "error", err)
		}

		// If step 1: backfill thread_id and parent_message_id on future sends
		if send.StepNumber == 1 {
			if _, err := execDB(cfg.DB, `UPDATE scheduled_sends
				SET thread_id = ?, parent_message_id = ?
				WHERE campaign_id = ? AND lead_id = ? AND step_number > 1 AND status = 'pending'`,
				threadID, storedMessageID, send.CampaignID, send.LeadID); err != nil {
				slog.Error("failed to backfill thread info",
					"send_id", send.ID, "campaign_id", send.CampaignID,
					"lead_id", send.LeadID, "error", err)
			}
		}

		if err := RebalancePendingSchedules(cfg.DB, []int64{send.AccountID}); err != nil {
			return nil, fmt.Errorf("rebalancing account %d after send: %w", send.AccountID, err)
		}

		dailyCounts[send.AccountID]++
		result.Sent++

		// Sleep between sends (skip for last send, dry-run, and tests)
		if i < len(dueSends)-1 && !cfg.DryRun && !cfg.NoSleep {
			gap := 90 + rand.Intn(51) // 90-140 seconds
			time.Sleep(time.Duration(gap) * time.Second)
		}
	}

	return result, nil
}

func loadActiveAccounts(db *sql.DB) ([]Account, error) {
	rows, err := queryDB(
		db,
		`SELECT
			id,
			email,
			daily_limit,
			status,
			provider,
			gws_config_dir,
			smtp_host,
			smtp_port,
			smtp_username,
			smtp_password_ref,
			smtp_tls_mode,
			imap_host,
			imap_port,
			imap_username,
			imap_password_ref,
			imap_tls_mode
		FROM accounts
		WHERE status = 'active'
			AND provider = 'gws'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(
			&a.ID,
			&a.Email,
			&a.DailyLimit,
			&a.Status,
			&a.Provider,
			&a.GWSConfigDir,
			&a.SMTPHost,
			&a.SMTPPort,
			&a.SMTPUsername,
			&a.SMTPPasswordRef,
			&a.SMTPTLSMode,
			&a.IMAPHost,
			&a.IMAPPort,
			&a.IMAPUsername,
			&a.IMAPPasswordRef,
			&a.IMAPTLSMode,
		); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

func preloadDailyCounts(db *sql.DB, now time.Time, tz *time.Location) (map[int64]int, error) {
	// Start of today in the configured timezone, converted to UTC for query
	localNow := now.In(tz)
	startOfDay := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, tz)
	today := startOfDay.UTC().Format(time.RFC3339)

	rows, err := queryDB(db, `
		SELECT account_id, COUNT(*)
		FROM events
		WHERE type = 'sent' AND timestamp >= ?
		GROUP BY account_id`, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[int64]int{}
	for rows.Next() {
		var accountID int64
		var count int
		rows.Scan(&accountID, &count)
		counts[accountID] = count
	}
	return counts, nil
}

type dueSend struct {
	ID              int64
	CampaignID      int64
	LeadID          int64
	AccountID       int64
	StepNumber      int
	VariantIndex    int
	ThreadID        string
	ParentMessageID string
}

func loadDueSends(db *sql.DB, now time.Time) ([]dueSend, error) {
	rows, err := queryDB(db, `
		SELECT ss.id, ss.campaign_id, ss.lead_id, ss.account_id,
			ss.step_number, ss.variant_index, ss.thread_id, ss.parent_message_id
		FROM scheduled_sends ss
		JOIN campaigns c ON ss.campaign_id = c.id
		WHERE ss.status = 'pending'
			AND ss.send_at <= ?
			AND c.status = 'active'
		ORDER BY ss.send_at`,
		now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sends []dueSend
	for rows.Next() {
		var s dueSend
		if err := rows.Scan(&s.ID, &s.CampaignID, &s.LeadID, &s.AccountID,
			&s.StepNumber, &s.VariantIndex, &s.ThreadID, &s.ParentMessageID); err != nil {
			return nil, err
		}
		sends = append(sends, s)
	}
	return sends, nil
}

func loadAllPendingSends(db *sql.DB) ([]dueSend, error) {
	rows, err := queryDB(db, `
		SELECT ss.id, ss.campaign_id, ss.lead_id, ss.account_id,
			ss.step_number, ss.variant_index, ss.thread_id, ss.parent_message_id
		FROM scheduled_sends ss
		JOIN campaigns c ON ss.campaign_id = c.id
		WHERE ss.status = 'pending'
			AND c.status = 'active'
		ORDER BY ss.send_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sends []dueSend
	for rows.Next() {
		var s dueSend
		if err := rows.Scan(&s.ID, &s.CampaignID, &s.LeadID, &s.AccountID,
			&s.StepNumber, &s.VariantIndex, &s.ThreadID, &s.ParentMessageID); err != nil {
			return nil, err
		}
		sends = append(sends, s)
	}
	return sends, nil
}

func refreshPendingSend(db *sql.DB, send dueSend) (dueSend, time.Time, bool, error) {
	var status string
	var sendAtStr string
	var campaignStatus string
	var threadID, parentMessageID string
	err := queryRowDB(db, `
		SELECT ss.status, ss.send_at, c.status, ss.thread_id, ss.parent_message_id
		FROM scheduled_sends ss
		JOIN campaigns c ON c.id = ss.campaign_id
		WHERE ss.id = ?`,
		send.ID,
	).Scan(&status, &sendAtStr, &campaignStatus, &threadID, &parentMessageID)
	if err == sql.ErrNoRows {
		return dueSend{}, time.Time{}, false, nil
	}
	if err != nil {
		return dueSend{}, time.Time{}, false, err
	}

	sendAt, err := parseDBTimestamp(sendAtStr)
	if err != nil {
		return dueSend{}, time.Time{}, false, err
	}

	if status != "pending" || campaignStatus != "active" {
		return dueSend{}, sendAt, false, nil
	}

	send.ThreadID = threadID
	send.ParentMessageID = parentMessageID
	return send, sendAt, true, nil
}

func isInSendWindow(db *sql.DB, campaignID, leadID int64, now time.Time) bool {
	var windowStart, windowEnd, tzName, sendDaysStr, leadEmail, customFields string
	err := queryRowDB(db, `
		SELECT c.send_window_start, c.send_window_end, c.timezone, c.send_days, l.email, l.custom_fields
		FROM campaigns c
		JOIN leads l ON l.id = ?
		WHERE c.id = ?`,
		leadID, campaignID,
	).Scan(&windowStart, &windowEnd, &tzName, &sendDaysStr, &leadEmail, &customFields)
	if err != nil {
		return true // default to allowing if we can't check
	}

	leadFields, err := buildLeadFields(leadEmail, "", "", "", "", customFields, true)
	if err != nil {
		return true
	}

	defaultTZ, err := time.LoadLocation(tzName)
	if err != nil {
		return true
	}

	tz, err := ResolveLeadScheduleTimezone(leadFields, defaultTZ)
	if err != nil {
		return true
	}

	localNow := now.In(tz)

	// Check day-of-week
	sendDays, err := ParseSendDays(sendDaysStr)
	if err != nil {
		return true
	}
	if !isDaySendable(localNow.Weekday(), sendDays) {
		return false
	}

	// Check time window
	start, err := parseTimeOfDay(windowStart)
	if err != nil {
		return true
	}
	end, err := parseTimeOfDay(windowEnd)
	if err != nil {
		return true
	}

	localHourMin := localNow.Hour()*60 + localNow.Minute()
	startMin := start.hour*60 + start.min
	endMin := end.hour*60 + end.min

	return localHourMin >= startMin && localHourMin < endMin
}

func loadSequenceForCampaign(db *sql.DB, campaignID int64) (*Sequence, error) {
	var seqFile, seqContent string
	err := queryRowDB(db, "SELECT sequence_file, sequence_content FROM campaigns WHERE id = ?",
		campaignID).Scan(&seqFile, &seqContent)
	if err != nil {
		return nil, err
	}
	// Prefer stored content; fall back to file path for pre-migration campaigns
	if seqContent != "" {
		return ParseSequenceFromBytes([]byte(seqContent))
	}
	return ParseSequence(seqFile)
}

func loadLeadFields(db *sql.DB, leadID int64) (map[string]string, error) {
	var email, firstName, lastName, company, domain, customFields string
	err := queryRowDB(db, `SELECT email, first_name, last_name, company, domain, custom_fields
		FROM leads WHERE id = ?`, leadID).
		Scan(&email, &firstName, &lastName, &company, &domain, &customFields)
	if err != nil {
		return nil, err
	}

	fields, err := buildLeadFields(email, firstName, lastName, company, domain, customFields, false)
	if err != nil {
		slog.Warn("failed to parse custom_fields JSON",
			"lead_id", leadID, "error", err)
		return map[string]string{
			"email":      email,
			"first_name": firstName,
			"last_name":  lastName,
			"company":    company,
			"domain":     domain,
		}, nil
	}

	return fields, nil
}

// markSendFailed marks a scheduled send as failed and inserts a failed event.
func markSendFailed(db *sql.DB, send dueSend, errorMsg string) {
	markSendStatus(db, send.ID, "failed", errorMsg)
	if _, err := execDB(db, `INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, metadata)
		VALUES (?, ?, ?, 'failed', ?, ?)`,
		send.CampaignID, send.LeadID, send.AccountID, send.StepNumber, errorMsg); err != nil {
		slog.Error("failed to insert failed event",
			"send_id", send.ID, "error", err)
	}
}

func markSendStatus(db *sql.DB, sendID int64, status string, errorMsg string) {
	if _, err := execDB(db, "UPDATE scheduled_sends SET status = ?, error_message = ? WHERE id = ?", status, errorMsg, sendID); err != nil {
		slog.Error("failed to mark send status",
			"send_id", sendID, "status", status, "error", err)
	}
}

func extractRFCMessageID(msg *GWSMessage) string {
	if msg == nil || msg.Headers == nil {
		return ""
	}
	return strings.TrimSpace(msg.Headers["Message-ID"])
}

// FormatTickResult returns a human-readable summary of a tick result.
func FormatTickResult(r *TickResult) string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[DRY RUN] ")
	}
	b.WriteString("Tick complete: ")
	parts := []string{}
	if r.Sent > 0 {
		parts = append(parts, fmt.Sprintf("%d sent", r.Sent))
	}
	if r.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", r.Failed))
	}
	if r.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", r.Skipped))
	}
	if r.RepliesDetected > 0 {
		parts = append(parts, fmt.Sprintf("%d replies", r.RepliesDetected))
	}
	if r.UnsubscribesDetected > 0 {
		parts = append(parts, fmt.Sprintf("%d unsubscribes", r.UnsubscribesDetected))
	}
	if r.BouncesDetected > 0 {
		parts = append(parts, fmt.Sprintf("%d bounces", r.BouncesDetected))
	}
	if len(parts) == 0 {
		b.WriteString("nothing to do")
	} else {
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}
