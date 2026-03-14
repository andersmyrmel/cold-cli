package internal

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// TickResult holds the summary of a tick invocation.
type TickResult struct {
	RepliesDetected int  `json:"replies_detected"`
	BouncesDetected int  `json:"bounces_detected"`
	Sent            int  `json:"sent"`
	Failed          int  `json:"failed"`
	Skipped         int  `json:"skipped"`
	DryRun          bool `json:"dry_run"`
}

// TickConfig holds configuration for a tick invocation.
type TickConfig struct {
	DB       *sql.DB
	GWS      GWSClient
	DryRun   bool
	Now      time.Time // injectable for testing
	NoSleep  bool      // skip inter-send sleep (for testing)
}

// Tick runs one tick cycle: poll replies, poll bounces, send due emails.
func Tick(cfg TickConfig) (*TickResult, error) {
	result := &TickResult{DryRun: cfg.DryRun}

	now := cfg.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Load active accounts
	accounts, err := loadActiveAccounts(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("loading accounts: %w", err)
	}

	if len(accounts) == 0 {
		return result, nil
	}

	// 1. Poll for replies
	replies, err := ProcessReplies(cfg.DB, cfg.GWS, accounts)
	if err != nil {
		// Log but don't abort — we still want to send
		fmt.Fprintf(os.Stderr, "warning: reply detection error: %v\n", err)
	}
	result.RepliesDetected = replies

	// 2. Poll for bounces
	bounces, err := ProcessBounces(cfg.DB, cfg.GWS, accounts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: bounce detection error: %v\n", err)
	}
	result.BouncesDetected = bounces

	// 3. Preload daily send counts per account
	dailyCounts, err := preloadDailyCounts(cfg.DB, now)
	if err != nil {
		return nil, fmt.Errorf("loading daily counts: %w", err)
	}

	// Build account lookup
	accountMap := map[int64]Account{}
	dailyLimits := map[int64]int{}
	for _, a := range accounts {
		accountMap[a.ID] = a
		dailyLimits[a.ID] = a.DailyLimit
	}

	// 4. Find due sends from active campaigns
	dueSends, err := loadDueSends(cfg.DB, now)
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
		// Check daily limit
		if dailyCounts[send.AccountID] >= dailyLimits[send.AccountID] {
			result.Skipped++
			continue
		}

		// Check send window
		if !isInSendWindow(cfg.DB, send.CampaignID, now) {
			result.Skipped++
			continue
		}

		// Load sequence if not cached
		seq, ok := seqCache[send.CampaignID]
		if !ok {
			seq, err = loadSequenceForCampaign(cfg.DB, send.CampaignID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: loading sequence for campaign %d: %v\n", send.CampaignID, err)
				result.Failed++
				continue
			}
			seqCache[send.CampaignID] = seq
		}

		// Load lead fields
		leadFields, err := loadLeadFields(cfg.DB, send.LeadID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: loading lead %d: %v\n", send.LeadID, err)
			result.Failed++
			continue
		}

		// Get account email
		account := accountMap[send.AccountID]

		// Build email
		emailParams := BuildEmailForSend(seq, send.StepNumber, send.VariantIndex, leadFields, account.Email)
		if emailParams.ToEmail == "" {
			fmt.Fprintf(os.Stderr, "warning: could not build email for send %d\n", send.ID)
			markSendStatus(cfg.DB, send.ID, "failed")
			result.Failed++
			continue
		}

		// For follow-ups, add threading headers
		if send.StepNumber > 1 && send.ParentMessageID != "" {
			// Look up the original subject from step 1
			originalSubject := getOriginalSubject(seq, send.VariantIndex)
			PrepareFollowUp(&emailParams, send.ParentMessageID, send.ThreadID, originalSubject)
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
		// For threaded replies, we need to include threadId in the send
		msgID, threadID, err := cfg.GWS.SendEmail(account.Email, emailParams.ToEmail, rawMsg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED send %d (step %d to %s via %s): %v\n",
				send.ID, send.StepNumber, emailParams.ToEmail, account.Email, err)
			markSendStatus(cfg.DB, send.ID, "failed")
			result.Failed++
			continue
		}

		// Validate response
		if msgID == "" || threadID == "" {
			fmt.Fprintf(os.Stderr, "FAILED send %d: gws returned empty message_id or thread_id\n", send.ID)
			markSendStatus(cfg.DB, send.ID, "failed")
			result.Failed++
			continue
		}

		// Mark sent
		cfg.DB.Exec(`UPDATE scheduled_sends SET status = 'sent', message_id = ?, sent_at = ?
			WHERE id = ?`, msgID, now.UTC().Format(time.RFC3339), send.ID)

		// Insert event
		cfg.DB.Exec(`INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, thread_id)
			VALUES (?, ?, ?, 'sent', ?, ?, ?)`,
			send.CampaignID, send.LeadID, send.AccountID, send.StepNumber, msgID, threadID)

		// If step 1: backfill thread_id and parent_message_id on future sends
		if send.StepNumber == 1 {
			cfg.DB.Exec(`UPDATE scheduled_sends
				SET thread_id = ?, parent_message_id = ?
				WHERE campaign_id = ? AND lead_id = ? AND step_number > 1 AND status = 'pending'`,
				threadID, msgID, send.CampaignID, send.LeadID)
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

// AcquireTickLock attempts to acquire the tick lock file.
// Returns the lock file (which must be closed to release) or an error.
func AcquireTickLock() (*os.File, error) {
	lockPath := filepath.Join(DataDir(), "tick.lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("tick already running")
	}

	return f, nil
}

func loadActiveAccounts(db *sql.DB) ([]Account, error) {
	rows, err := db.Query("SELECT id, email, daily_limit, status FROM accounts WHERE status = 'active'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Email, &a.DailyLimit, &a.Status); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

func preloadDailyCounts(db *sql.DB, now time.Time) (map[int64]int, error) {
	// Start of today in UTC
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	rows, err := db.Query(`
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
	rows, err := db.Query(`
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

func isInSendWindow(db *sql.DB, campaignID int64, now time.Time) bool {
	var windowStart, windowEnd, tzName string
	err := db.QueryRow("SELECT send_window_start, send_window_end, timezone FROM campaigns WHERE id = ?",
		campaignID).Scan(&windowStart, &windowEnd, &tzName)
	if err != nil {
		return true // default to allowing if we can't check
	}

	tz, err := time.LoadLocation(tzName)
	if err != nil {
		return true
	}

	localNow := now.In(tz)
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
	var seqFile string
	err := db.QueryRow("SELECT sequence_file FROM campaigns WHERE id = ?", campaignID).Scan(&seqFile)
	if err != nil {
		return nil, err
	}
	return ParseSequence(seqFile)
}

func loadLeadFields(db *sql.DB, leadID int64) (map[string]string, error) {
	var email, firstName, lastName, company, domain, customFields string
	err := db.QueryRow(`SELECT email, first_name, last_name, company, domain, custom_fields
		FROM leads WHERE id = ?`, leadID).
		Scan(&email, &firstName, &lastName, &company, &domain, &customFields)
	if err != nil {
		return nil, err
	}

	fields := map[string]string{
		"email":      email,
		"first_name": firstName,
		"last_name":  lastName,
		"company":    company,
		"domain":     domain,
	}

	// TODO: parse custom_fields JSON and merge into fields

	return fields, nil
}

func markSendStatus(db *sql.DB, sendID int64, status string) {
	db.Exec("UPDATE scheduled_sends SET status = ? WHERE id = ?", status, sendID)
}

func getOriginalSubject(seq *Sequence, variantIndex int) string {
	if len(seq.Steps) == 0 {
		return ""
	}
	step := seq.Steps[0]
	if variantIndex > 0 && variantIndex <= len(step.Variants) {
		v := step.Variants[variantIndex-1]
		if v.Subject != "" {
			return v.Subject
		}
	}
	return step.Subject
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
