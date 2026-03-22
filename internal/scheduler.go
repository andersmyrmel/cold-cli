package internal

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Sequence represents a parsed sequence YAML file.
type Sequence struct {
	Name     string         `yaml:"name"`
	Defaults SequenceDefaults `yaml:"defaults"`
	Steps    []SequenceStep `yaml:"steps"`
}

type SequenceDefaults struct {
	FromName string `yaml:"from_name"`
}

type SequenceStep struct {
	Step     int              `yaml:"step"`
	Delay    int              `yaml:"delay"` // days after previous step
	Subject  string           `yaml:"subject"`
	Body     string           `yaml:"body"`
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
	CampaignID    int64
	AccountIDs    []int64
	Leads         []LeadForSchedule
	Sequence      *Sequence
	SendWindowStart string // "09:00"
	SendWindowEnd   string // "17:00"
	SendDays      []time.Weekday
	Timezone      *time.Location
	MinGapSeconds int
	MaxGapSeconds int
	StartTime     time.Time // when the campaign starts (typically now)
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

	for leadIdx, lead := range cfg.Leads {
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
				sendAt = cfg.StartTime
				// Add per-lead offset: lead position * random gap
				gapSec := cfg.MinGapSeconds
				if cfg.MaxGapSeconds > cfg.MinGapSeconds {
					gapSec += rand.Intn(cfg.MaxGapSeconds - cfg.MinGapSeconds)
				}
				sendAt = sendAt.Add(time.Duration(leadIdx*gapSec) * time.Second)
			} else {
				// Step N: previous step send_at + delay days
				sendAt = prevSendAt.AddDate(0, 0, step.Delay)
			}

			// Clamp to send window and skip non-send days
			sendAt = clampToWindow(sendAt, windowStart, windowEnd, cfg.SendDays, cfg.Timezone)

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

// ValidateLeadFields checks that every lead has non-empty values for all required placeholders.
func ValidateLeadFields(leads []LeadRecord, placeholders []string) error {
	var errors []string
	for _, lead := range leads {
		var missing []string
		for _, p := range placeholders {
			if val, ok := lead.Fields[p]; !ok || val == "" {
				missing = append(missing, "{{"+p+"}}")
			}
		}
		if len(missing) > 0 {
			errors = append(errors, fmt.Sprintf("  %s: missing %s", lead.Fields["email"], strings.Join(missing, ", ")))
		}
	}
	if len(errors) > 0 {
		return fmt.Errorf("leads missing required fields:\n%s\n\nSequence uses: %s",
			strings.Join(errors, "\n"),
			strings.Join(wrapPlaceholders(placeholders), ", "),
		)
	}
	return nil
}

func wrapPlaceholders(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = "{{" + p + "}}"
	}
	return out
}
