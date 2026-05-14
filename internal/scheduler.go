package internal

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Sequence represents a parsed sequence YAML file.
type Sequence struct {
	Name     string           `yaml:"name"`
	Defaults SequenceDefaults `yaml:"defaults"`
	Steps    []SequenceStep   `yaml:"steps"`
}

type SequenceDefaults struct {
	FromName string `yaml:"from_name"`
}

type SequenceStep struct {
	Step     int               `yaml:"step"`
	Delay    int               `yaml:"delay"` // days after previous step
	Subject  string            `yaml:"subject"`
	Body     string            `yaml:"body"`
	Variants []SequenceVariant `yaml:"variants"`
}

type SequenceVariant struct {
	Subject string `yaml:"subject"`
	Body    string `yaml:"body"`
}

// ParseSequence reads and parses a sequence YAML file.
func ParseSequence(path string) (*Sequence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading sequence file: %w", err)
	}
	return ParseSequenceFromBytes(data)
}

// ParseSequenceFromBytes parses sequence YAML from bytes.
func ParseSequenceFromBytes(data []byte) (*Sequence, error) {
	var seq Sequence
	if err := yaml.Unmarshal(data, &seq); err != nil {
		return nil, fmt.Errorf("parsing sequence YAML: %w", err)
	}

	if len(seq.Steps) == 0 {
		return nil, fmt.Errorf("sequence has no steps")
	}

	for i, step := range seq.Steps {
		if step.Body == "" {
			return nil, fmt.Errorf("step %d has no body", i+1)
		}
		if i == 0 && step.Subject == "" && len(step.Variants) == 0 {
			return nil, fmt.Errorf("step 1 must have a subject (it starts a new thread)")
		}
	}

	return &seq, nil
}

// CollectPlaceholders returns all unique placeholders used across all steps and variants.
func (s *Sequence) CollectPlaceholders() []string {
	seen := map[string]bool{}
	var all []string

	collect := func(text string) {
		for _, p := range ExtractPlaceholders(text) {
			if !seen[p] {
				seen[p] = true
				all = append(all, p)
			}
		}
	}

	for _, step := range s.Steps {
		collect(step.Subject)
		collect(step.Body)
		for _, v := range step.Variants {
			collect(v.Subject)
			collect(v.Body)
		}
	}

	return all
}

// ScheduleConfig holds parameters for schedule computation.
type ScheduleConfig struct {
	CampaignID      int64
	AccountIDs      []int64
	Leads           []LeadForSchedule
	Sequence        *Sequence
	StartDate       string
	SendWindowStart string // "09:00"
	SendWindowEnd   string // "17:00"
	SendDays        []time.Weekday
	Timezone        *time.Location
	MinGapSeconds   int
	MaxGapSeconds   int
	StartTime       time.Time // when the campaign starts (typically now)
}

// LeadForSchedule is the minimal lead info needed for scheduling.
type LeadForSchedule struct {
	ID     int64
	Fields map[string]string
}

// ScheduledSendRow is a row to insert into scheduled_sends.
type ScheduledSendRow struct {
	CampaignID   int64
	LeadID       int64
	AccountID    int64
	StepNumber   int
	VariantIndex int
	SendAt       time.Time
}

// ComputeSchedule generates all scheduled_sends rows for a campaign.
func ComputeSchedule(cfg ScheduleConfig) ([]ScheduledSendRow, error) {
	if len(cfg.AccountIDs) == 0 {
		return nil, fmt.Errorf("no accounts provided")
	}
	if len(cfg.Leads) == 0 {
		return nil, fmt.Errorf("no leads provided")
	}
	if len(cfg.Sequence.Steps) == 0 {
		return nil, fmt.Errorf("sequence has no steps")
	}

	windowStart, err := parseTimeOfDay(cfg.SendWindowStart)
	if err != nil {
		return nil, fmt.Errorf("invalid send_window_start: %w", err)
	}
	windowEnd, err := parseTimeOfDay(cfg.SendWindowEnd)
	if err != nil {
		return nil, fmt.Errorf("invalid send_window_end: %w", err)
	}

	var rows []ScheduledSendRow
	baseNow := timeNow()

	for leadIdx, lead := range cfg.Leads {
		leadTZ, err := ResolveLeadScheduleTimezone(lead.Fields, cfg.Timezone)
		if err != nil {
			return nil, err
		}

		leadStart := cfg.StartTime.In(leadTZ)
		if strings.TrimSpace(cfg.StartDate) != "" {
			leadStart, err = campaignStartAnchor(baseNow.In(leadTZ), cfg.StartDate, windowStart, leadTZ)
			if err != nil {
				return nil, fmt.Errorf("invalid start date %q (expected YYYY-MM-DD): %w", cfg.StartDate, err)
			}
		}

		// Round-robin: all steps for one lead use the same account
		accountID := cfg.AccountIDs[leadIdx%len(cfg.AccountIDs)]

		var prevSendAt time.Time

		for stepIdx, step := range cfg.Sequence.Steps {
			// Compute variant index
			variantIndex := 0
			totalVariants := 1 + len(step.Variants) // base + variants
			if totalVariants > 1 {
				variantIndex = leadIdx % totalVariants
			}

			// Compute send_at
			var sendAt time.Time
			if stepIdx == 0 {
				// Step 1: start time + offset based on lead position
				// Spread leads across the first send window using jitter
				sendAt = leadStart
				// Add per-lead offset: lead position * random gap
				gapSec := cfg.MinGapSeconds
				if cfg.MaxGapSeconds > cfg.MinGapSeconds {
					gapSec += rand.Intn(cfg.MaxGapSeconds - cfg.MinGapSeconds)
				}
				sendAt = sendAt.Add(time.Duration(leadIdx*gapSec) * time.Second)
			} else {
				sendAt = nextScheduledTime(prevSendAt, step.Delay, windowStart, windowEnd, cfg.SendDays, leadTZ)
			}

			if stepIdx == 0 {
				// Step 1 still needs clamping after applying the per-lead offset.
				sendAt = clampToWindow(sendAt, windowStart, windowEnd, cfg.SendDays, leadTZ)
			}

			rows = append(rows, ScheduledSendRow{
				CampaignID:   cfg.CampaignID,
				LeadID:       lead.ID,
				AccountID:    accountID,
				StepNumber:   step.Step,
				VariantIndex: variantIndex,
				SendAt:       sendAt,
			})

			prevSendAt = sendAt
		}
	}

	return rows, nil
}

type campaignScheduleRules struct {
	CampaignID      int64
	StartDate       string
	SendWindowStart string
	SendWindowEnd   string
	SendDays        []time.Weekday
	Timezone        *time.Location
	WindowStartTOD  timeOfDay
	WindowEndTOD    timeOfDay
	StepByNumber    map[int]SequenceStep
}

type accountScheduleRow struct {
	ID         int64
	CampaignID int64
	LeadID     int64
	LeadEmail  string
	AccountID  int64
	StepNumber int
	Status     string
	SendAt     time.Time
	SentAt     time.Time
	HasSentAt  bool
	ScheduleTZ string
}

type leadScheduleQueue struct {
	rules      *campaignScheduleRules
	rows       []accountScheduleRow
	nextIdx    int
	nextAt     time.Time
	hasNext    bool
	anchorTime time.Time
}

type pendingScheduleCandidate struct {
	queue *leadScheduleQueue
	row   accountScheduleRow
	at    time.Time
}

// RebalancePendingSchedules rewrites pending send_at timestamps for the affected
// accounts so daily limits are respected across all active and draft campaigns.
func RebalancePendingSchedules(db *sql.DB, accountIDs []int64) error {
	accountIDs = uniqueSortedInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return nil
	}

	tx, err := beginTx(db)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	if err := rebalancePendingSchedulesTx(tx, accountIDs); err != nil {
		return err
	}

	return tx.Commit()
}

func rebalancePendingSchedulesTx(tx *Tx, accountIDs []int64) error {
	accountIDs = uniqueSortedInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return nil
	}

	limits, err := loadAccountDailyLimitsTx(tx, accountIDs)
	if err != nil {
		return fmt.Errorf("loading account daily limits: %w", err)
	}
	if len(limits) == 0 {
		return nil
	}

	rows, rulesByCampaign, err := loadAccountScheduleRowsTx(tx, accountIDs)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	usageByAccount, err := loadSentUsageByAccountTx(tx, accountIDs)
	if err != nil {
		return fmt.Errorf("loading sent usage: %w", err)
	}

	rowsByAccountLead := make(map[int64]map[string][]accountScheduleRow)
	for _, row := range rows {
		if _, ok := rowsByAccountLead[row.AccountID]; !ok {
			rowsByAccountLead[row.AccountID] = map[string][]accountScheduleRow{}
		}
		key := scheduleLeadKey(row.CampaignID, row.LeadID)
		rowsByAccountLead[row.AccountID][key] = append(rowsByAccountLead[row.AccountID][key], row)
	}

	for _, accountID := range accountIDs {
		leadRows := rowsByAccountLead[accountID]
		if len(leadRows) == 0 {
			continue
		}

		var queues []*leadScheduleQueue
		for _, rows := range leadRows {
			rules := rulesByCampaign[rows[0].CampaignID]
			queue, err := newLeadScheduleQueue(rows, rules)
			if err != nil {
				return err
			}
			if queue.hasNext {
				queues = append(queues, queue)
			}
		}

		usage := usageByAccount[accountID]
		if usage == nil {
			usage = map[string]int{}
			usageByAccount[accountID] = usage
		}

		limit := limits[accountID]
		if limit < 1 {
			limit = 1
		}

		for {
			var candidates []pendingScheduleCandidate
			for _, queue := range queues {
				if !queue.hasNext {
					continue
				}
				candidates = append(candidates, pendingScheduleCandidate{
					queue: queue,
					row:   queue.rows[queue.nextIdx],
					at:    queue.nextAt,
				})
			}
			if len(candidates) == 0 {
				break
			}

			sort.Slice(candidates, func(i, j int) bool {
				left := candidates[i]
				right := candidates[j]
				if !left.at.Equal(right.at) {
					return left.at.Before(right.at)
				}
				if !left.row.SendAt.Equal(right.row.SendAt) {
					return left.row.SendAt.Before(right.row.SendAt)
				}
				if left.row.CampaignID != right.row.CampaignID {
					return left.row.CampaignID < right.row.CampaignID
				}
				if left.row.LeadID != right.row.LeadID {
					return left.row.LeadID < right.row.LeadID
				}
				if left.row.StepNumber != right.row.StepNumber {
					return left.row.StepNumber < right.row.StepNumber
				}
				return left.row.ID < right.row.ID
			})

			candidate := candidates[0]
			assignedAt := allocateDailyLimitedSendAt(candidate.at, candidate.queue.rules, usage, limit)
			if _, err := tx.Exec(
				"UPDATE scheduled_sends SET send_at = ? WHERE id = ?",
				assignedAt.UTC().Format(time.RFC3339), candidate.row.ID,
			); err != nil {
				return fmt.Errorf("updating send %d: %w", candidate.row.ID, err)
			}

			if err := candidate.queue.advance(assignedAt); err != nil {
				return err
			}
		}
	}

	return nil
}

func loadAccountDailyLimitsTx(tx *Tx, accountIDs []int64) (map[int64]int, error) {
	query, args := accountIDInClauseQuery(
		"SELECT id, daily_limit FROM accounts WHERE id IN (%s)",
		accountIDs,
	)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	limits := map[int64]int{}
	for rows.Next() {
		var accountID int64
		var limit int
		if err := rows.Scan(&accountID, &limit); err != nil {
			return nil, err
		}
		limits[accountID] = limit
	}
	return limits, rows.Err()
}

func loadAccountScheduleRowsTx(tx *Tx, accountIDs []int64) ([]accountScheduleRow, map[int64]*campaignScheduleRules, error) {
	query, args := accountIDInClauseQuery(`
		SELECT ss.id, ss.campaign_id, ss.lead_id, l.email, ss.account_id, ss.step_number, ss.status,
			CAST(ss.send_at AS TEXT), CASE WHEN ss.sent_at IS NULL THEN NULL ELSE CAST(ss.sent_at AS TEXT) END, c.sequence_file, c.sequence_content, c.start_date,
			c.send_window_start, c.send_window_end, c.send_days, c.timezone, l.custom_fields
		FROM scheduled_sends ss
		JOIN campaigns c ON c.id = ss.campaign_id
		JOIN leads l ON l.id = ss.lead_id
		WHERE ss.account_id IN (%s)
		  AND c.status IN ('active', 'draft')
		ORDER BY ss.account_id, ss.campaign_id, ss.lead_id, ss.step_number, ss.id`,
		accountIDs,
	)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var scheduledRows []accountScheduleRow
	rulesByCampaign := map[int64]*campaignScheduleRules{}
	for rows.Next() {
		var row accountScheduleRow
		var sendAtStr string
		var sentAtStr sql.NullString
		var seqFile, seqContent, startDate, sendWindowStart, sendWindowEnd, sendDaysStr, timezoneName string
		var customFields string
		if err := rows.Scan(
			&row.ID, &row.CampaignID, &row.LeadID, &row.LeadEmail, &row.AccountID, &row.StepNumber, &row.Status,
			&sendAtStr, &sentAtStr, &seqFile, &seqContent, &startDate, &sendWindowStart, &sendWindowEnd, &sendDaysStr, &timezoneName, &customFields,
		); err != nil {
			return nil, nil, err
		}

		leadFields, err := buildLeadFields(row.LeadEmail, "", "", "", "", customFields, true)
		if err != nil {
			return nil, nil, fmt.Errorf("loading lead %s scheduling overrides: %w", row.LeadEmail, err)
		}
		row.ScheduleTZ = strings.TrimSpace(leadFields[ScheduleTimezoneField])

		row.SendAt, err = parseDBTimestamp(sendAtStr)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing send_at for send %d: %w", row.ID, err)
		}
		if sentAtStr.Valid && sentAtStr.String != "" {
			row.SentAt, err = parseDBTimestamp(sentAtStr.String)
			if err != nil {
				return nil, nil, fmt.Errorf("parsing sent_at for send %d: %w", row.ID, err)
			}
			row.HasSentAt = true
		}
		scheduledRows = append(scheduledRows, row)

		if _, ok := rulesByCampaign[row.CampaignID]; ok {
			continue
		}

		seq, err := loadStoredCampaignSequence(seqFile, seqContent)
		if err != nil {
			return nil, nil, fmt.Errorf("loading sequence for campaign %d: %w", row.CampaignID, err)
		}
		sendDays, err := ParseSendDays(sendDaysStr)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing send_days for campaign %d: %w", row.CampaignID, err)
		}
		tz, err := time.LoadLocation(timezoneName)
		if err != nil {
			return nil, nil, fmt.Errorf("loading timezone %q for campaign %d: %w", timezoneName, row.CampaignID, err)
		}
		windowStartTOD, err := parseTimeOfDay(sendWindowStart)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing send_window_start for campaign %d: %w", row.CampaignID, err)
		}
		windowEndTOD, err := parseTimeOfDay(sendWindowEnd)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing send_window_end for campaign %d: %w", row.CampaignID, err)
		}

		stepByNumber := make(map[int]SequenceStep, len(seq.Steps))
		for _, step := range seq.Steps {
			stepByNumber[step.Step] = step
		}

		rulesByCampaign[row.CampaignID] = &campaignScheduleRules{
			CampaignID:      row.CampaignID,
			StartDate:       startDate,
			SendWindowStart: sendWindowStart,
			SendWindowEnd:   sendWindowEnd,
			SendDays:        sendDays,
			Timezone:        tz,
			WindowStartTOD:  windowStartTOD,
			WindowEndTOD:    windowEndTOD,
			StepByNumber:    stepByNumber,
		}
	}

	return scheduledRows, rulesByCampaign, rows.Err()
}

func loadSentUsageByAccountTx(tx *Tx, accountIDs []int64) (map[int64]map[string]int, error) {
	query, args := accountIDInClauseQuery(
		"SELECT account_id, timestamp FROM events WHERE type = 'sent' AND account_id IN (%s)",
		accountIDs,
	)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	usage := map[int64]map[string]int{}
	for rows.Next() {
		var accountID int64
		var timestamp string
		if err := rows.Scan(&accountID, &timestamp); err != nil {
			return nil, err
		}

		sentAt, err := parseDBTimestamp(timestamp)
		if err != nil {
			return nil, fmt.Errorf("parsing sent event timestamp %q: %w", timestamp, err)
		}

		if usage[accountID] == nil {
			usage[accountID] = map[string]int{}
		}
		usage[accountID][dailyLimitDateKey(sentAt)]++
	}

	return usage, rows.Err()
}

func newLeadScheduleQueue(rows []accountScheduleRow, rules *campaignScheduleRules) (*leadScheduleQueue, error) {
	queueRules, err := effectiveScheduleRulesForLead(rules, rows[0])
	if err != nil {
		return nil, err
	}

	queue := &leadScheduleQueue{
		rules:   queueRules,
		rows:    rows,
		nextIdx: -1,
	}

	var prevAnchor time.Time
	var havePrev bool
	for idx, row := range rows {
		if row.HasSentAt {
			prevAnchor = row.SentAt.In(queueRules.Timezone)
			havePrev = true
			continue
		}
		if row.Status != "pending" {
			continue
		}

		nextAt, err := firstPendingAnchorForRow(row, prevAnchor, havePrev, queueRules)
		if err != nil {
			return nil, err
		}
		queue.nextIdx = idx
		queue.nextAt = nextAt
		queue.anchorTime = nextAt
		queue.hasNext = true
		return queue, nil
	}

	return queue, nil
}

func effectiveScheduleRulesForLead(base *campaignScheduleRules, row accountScheduleRow) (*campaignScheduleRules, error) {
	fields := map[string]string{
		"email":               row.LeadEmail,
		ScheduleTimezoneField: row.ScheduleTZ,
	}
	tz, err := ResolveLeadScheduleTimezone(fields, base.Timezone)
	if err != nil {
		return nil, err
	}

	clone := *base
	clone.Timezone = tz
	return &clone, nil
}

func firstPendingAnchorForRow(row accountScheduleRow, prevAnchor time.Time, havePrev bool, rules *campaignScheduleRules) (time.Time, error) {
	if !havePrev {
		existingAnchor := clampToWindow(row.SendAt.In(rules.Timezone), rules.WindowStartTOD, rules.WindowEndTOD, rules.SendDays, rules.Timezone)
		if row.StepNumber == 1 && strings.TrimSpace(rules.StartDate) != "" {
			freshAnchor, err := campaignStartAnchor(timeNow().In(rules.Timezone), rules.StartDate, rules.WindowStartTOD, rules.Timezone)
			if err != nil {
				return time.Time{}, err
			}
			if shouldPullStep1AnchorBack(existingAnchor, freshAnchor) {
				return freshAnchor, nil
			}
		}
		return existingAnchor, nil
	}

	step, ok := rules.StepByNumber[row.StepNumber]
	if !ok {
		return time.Time{}, fmt.Errorf("campaign %d is missing step %d", row.CampaignID, row.StepNumber)
	}
	return nextScheduledTime(prevAnchor, step.Delay, rules.WindowStartTOD, rules.WindowEndTOD, rules.SendDays, rules.Timezone), nil
}

func shouldPullStep1AnchorBack(existingAnchor, freshAnchor time.Time) bool {
	if !existingAnchor.After(freshAnchor) {
		return false
	}

	// Preserve normal jitter and daily-limit deferrals. Only repair clearly stale
	// unsent step-1 rows, such as schedules stranded months/years in the future.
	return existingAnchor.After(freshAnchor.AddDate(0, 0, 30))
}

func (q *leadScheduleQueue) advance(assignedAt time.Time) error {
	q.anchorTime = assignedAt.In(q.rules.Timezone)
	for idx := q.nextIdx + 1; idx < len(q.rows); idx++ {
		row := q.rows[idx]
		if row.Status != "pending" {
			continue
		}

		step, ok := q.rules.StepByNumber[row.StepNumber]
		if !ok {
			return fmt.Errorf("campaign %d is missing step %d", row.CampaignID, row.StepNumber)
		}

		q.nextIdx = idx
		q.nextAt = nextScheduledTime(q.anchorTime, step.Delay, q.rules.WindowStartTOD, q.rules.WindowEndTOD, q.rules.SendDays, q.rules.Timezone)
		q.hasNext = true
		return nil
	}

	q.hasNext = false
	return nil
}

func allocateDailyLimitedSendAt(earliest time.Time, rules *campaignScheduleRules, usage map[string]int, dailyLimit int) time.Time {
	sendAt := clampToWindow(earliest.In(rules.Timezone), rules.WindowStartTOD, rules.WindowEndTOD, rules.SendDays, rules.Timezone)
	for usage[dailyLimitDateKey(sendAt)] >= dailyLimit {
		sendAt = nextScheduledTime(sendAt, 1, rules.WindowStartTOD, rules.WindowEndTOD, rules.SendDays, rules.Timezone)
	}
	usage[dailyLimitDateKey(sendAt)]++
	return sendAt
}

func accountIDInClauseQuery(format string, accountIDs []int64) (string, []any) {
	placeholders := make([]string, len(accountIDs))
	args := make([]any, len(accountIDs))
	for i, accountID := range accountIDs {
		placeholders[i] = "?"
		args[i] = accountID
	}
	return fmt.Sprintf(format, strings.Join(placeholders, ",")), args
}

func uniqueSortedInt64s(values []int64) []int64 {
	if len(values) == 0 {
		return nil
	}
	seen := map[int64]bool{}
	var unique []int64
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

func scheduleLeadKey(campaignID, leadID int64) string {
	return fmt.Sprintf("%d:%d", campaignID, leadID)
}

func dailyLimitDateKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// timeOfDay represents an hour:minute pair.
type timeOfDay struct {
	hour, min int
}

func parseTimeOfDay(s string) (timeOfDay, error) {
	var h, m int
	n, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err != nil || n != 2 {
		return timeOfDay{}, fmt.Errorf("expected HH:MM, got %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return timeOfDay{}, fmt.Errorf("invalid time %q", s)
	}
	return timeOfDay{hour: h, min: m}, nil
}

// clampToWindow adjusts sendAt so it falls within the send window on a valid send day.
func clampToWindow(sendAt time.Time, start, end timeOfDay, sendDays []time.Weekday, tz *time.Location) time.Time {
	t := sendAt.In(tz)

	// If before window start, move to window start same day
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), start.hour, start.min, 0, 0, tz)
	dayEnd := time.Date(t.Year(), t.Month(), t.Day(), end.hour, end.min, 0, 0, tz)

	if t.Before(dayStart) {
		t = dayStart
	} else if t.After(dayEnd) || t.Equal(dayEnd) {
		// Past window end — move to next day's window start
		t = dayStart.AddDate(0, 0, 1)
	}

	// Skip non-send days
	for !isDaySendable(t.Weekday(), sendDays) {
		t = time.Date(t.Year(), t.Month(), t.Day(), start.hour, start.min, 0, 0, tz)
		t = t.AddDate(0, 0, 1)
	}

	return t
}

func nextScheduledTime(prevSendAt time.Time, delayDays int, start, end timeOfDay, sendDays []time.Weekday, tz *time.Location) time.Time {
	return clampToWindow(prevSendAt.AddDate(0, 0, delayDays), start, end, sendDays, tz)
}

func isDaySendable(day time.Weekday, sendDays []time.Weekday) bool {
	for _, d := range sendDays {
		if d == day {
			return true
		}
	}
	return false
}

// ParseSendDays converts a comma-separated day string to weekday slice.
// Accepts numbers (0=Sun,1=Mon,...,6=Sat) or names (sun,mon,...,sat).
func ParseSendDays(s string) ([]time.Weekday, error) {
	nameToDay := map[string]time.Weekday{
		"sun": time.Sunday, "sunday": time.Sunday,
		"mon": time.Monday, "monday": time.Monday,
		"tue": time.Tuesday, "tuesday": time.Tuesday,
		"wed": time.Wednesday, "wednesday": time.Wednesday,
		"thu": time.Thursday, "thursday": time.Thursday,
		"fri": time.Friday, "friday": time.Friday,
		"sat": time.Saturday, "saturday": time.Saturday,
	}

	parts := strings.Split(s, ",")
	var days []time.Weekday
	for _, p := range parts {
		p = strings.TrimSpace(p)
		lower := strings.ToLower(p)

		// Try name first
		if d, ok := nameToDay[lower]; ok {
			days = append(days, d)
			continue
		}

		// Try number
		var d int
		if _, err := fmt.Sscanf(p, "%d", &d); err != nil {
			return nil, fmt.Errorf("invalid day %q (use 0-6 or day names like mon,tue,wed)", p)
		}
		if d < 0 || d > 6 {
			return nil, fmt.Errorf("day %d out of range (0-6)", d)
		}
		days = append(days, time.Weekday(d))
	}
	return days, nil
}

// ValidateLeadFields checks that every placeholder in the sequence maps to a known field
// and that every lead has non-empty values for all required placeholders.
// Returns alias-mapping warnings (if any) and an error if validation fails.
func ValidateLeadFields(leads []LeadRecord, placeholders []string) ([]string, error) {
	if len(leads) == 0 || len(placeholders) == 0 {
		return nil, nil
	}

	// Build the set of available fields: built-in fields + CSV columns
	available := make(map[string]bool)
	var availableList []string
	for _, f := range BuiltinFields {
		if !available[f] {
			available[f] = true
			availableList = append(availableList, f)
		}
	}
	for k := range leads[0].Fields {
		if !available[k] {
			available[k] = true
			availableList = append(availableList, k)
		}
	}
	sort.Strings(availableList)

	// Phase 1: resolve aliases and check each placeholder maps to an available field
	var warnings []string
	resolved := make([]string, len(placeholders))
	var unresolvable []string

	for i, p := range placeholders {
		canonical := ResolveAlias(p)
		if canonical != p {
			warnings = append(warnings, fmt.Sprintf("template variable {{%s}} mapped to %s", p, canonical))
		}
		if available[canonical] {
			resolved[i] = canonical
		} else {
			suggestion := SuggestField(canonical, availableList)
			msg := fmt.Sprintf("template variable {{%s}} has no matching field", p)
			if suggestion != "" {
				msg += fmt.Sprintf(". Did you mean %q?", suggestion)
			}
			unresolvable = append(unresolvable, msg)
		}
	}

	if len(unresolvable) > 0 {
		return nil, fmt.Errorf("%s\nAvailable fields: %s",
			strings.Join(unresolvable, "\n"),
			strings.Join(availableList, ", "))
	}

	// Phase 2: check each lead has non-empty values for resolved placeholders
	var errors []string
	for _, lead := range leads {
		var missing []string
		for i, rp := range resolved {
			if val, ok := lead.Fields[rp]; !ok || val == "" {
				missing = append(missing, "{{"+placeholders[i]+"}}")
			}
		}
		if len(missing) > 0 {
			errors = append(errors, fmt.Sprintf("  %s: missing %s", lead.Fields["email"], strings.Join(missing, ", ")))
		}
	}
	if len(errors) > 0 {
		return warnings, fmt.Errorf("leads missing required fields:\n%s\n\nSequence uses: %s",
			strings.Join(errors, "\n"),
			strings.Join(wrapPlaceholders(placeholders), ", "),
		)
	}

	return warnings, nil
}

func wrapPlaceholders(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = "{{" + p + "}}"
	}
	return out
}
