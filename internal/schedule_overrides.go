package internal

import (
	"fmt"
	"strings"
	"time"
)

const ScheduleTimezoneField = "schedule_timezone"

// ValidateLeadScheduleOverrides checks optional per-lead scheduling override fields.
func ValidateLeadScheduleOverrides(leads []LeadRecord) error {
	var errors []string
	for _, lead := range leads {
		tzName := strings.TrimSpace(lead.Fields[ScheduleTimezoneField])
		if tzName == "" {
			continue
		}
		if _, err := time.LoadLocation(tzName); err != nil {
			email := lead.Fields["email"]
			errors = append(errors, fmt.Sprintf("  %s: invalid %s %q", email, ScheduleTimezoneField, tzName))
		}
	}
	if len(errors) == 0 {
		return nil
	}
	return fmt.Errorf("leads have invalid scheduling overrides:\n%s", strings.Join(errors, "\n"))
}

// ResolveLeadScheduleTimezone returns the lead's effective scheduling timezone.
func ResolveLeadScheduleTimezone(fields map[string]string, defaultTZ *time.Location) (*time.Location, error) {
	tzName := strings.TrimSpace(fields[ScheduleTimezoneField])
	if tzName == "" {
		return defaultTZ, nil
	}

	tz, err := time.LoadLocation(tzName)
	if err != nil {
		email := fields["email"]
		if email == "" {
			email = "(unknown lead)"
		}
		return nil, fmt.Errorf("lead %s has invalid %s %q: %w", email, ScheduleTimezoneField, tzName, err)
	}

	return tz, nil
}
