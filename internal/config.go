package internal

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultTimezone    string `yaml:"default_timezone"`
	DefaultDailyLimit  int    `yaml:"default_daily_limit"`
	MinGapSeconds      int    `yaml:"min_gap_seconds"`
	MaxGapSeconds      int    `yaml:"max_gap_seconds"`
	SendWindowStart    string `yaml:"send_window_start"`
	SendWindowEnd      string `yaml:"send_window_end"`
	SendDays           string `yaml:"send_days"`
	UnsubscribeHeader  bool   `yaml:"unsubscribe_header"`
	UnsubscribeSubject string `yaml:"unsubscribe_subject"`
}

var DefaultConfig = Config{
	DefaultTimezone:    "America/New_York",
	DefaultDailyLimit:  50,
	MinGapSeconds:      90,
	MaxGapSeconds:      140,
	SendWindowStart:    "09:00",
	SendWindowEnd:      "17:00",
	SendDays:           "1,2,3,4,5",
	UnsubscribeSubject: "Unsubscribe",
}

func ConfigPath() string {
	return filepath.Join(DataDir(), "config.yml")
}

func LoadConfig() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig
			return &cfg, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := DefaultConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

func WriteDefaultConfig(path string) error {
	content := `# cold-cli configuration
# These are defaults for new campaigns. Per-account daily_limit is set via:
#   cold-cli account add <email> --daily-limit N
# The account's daily_limit is the one used by schedule rebalance, preview,
# warnings, and tick. Sent counts still come from the events table.

default_timezone: America/New_York
default_daily_limit: 50
min_gap_seconds: 90
max_gap_seconds: 140
send_window_start: "09:00"
send_window_end: "17:00"

# Send days: 0=Sunday, 1=Monday, 2=Tuesday, ..., 6=Saturday
send_days: "1,2,3,4,5"

# Add List-Unsubscribe header to emails (off by default — not needed for cold email from personal Gmail)
# Enable only if sending bulk/marketing campaigns that require it
unsubscribe_header: false

# Subject line used for detecting unsubscribe replies (always active regardless of header setting)
unsubscribe_subject: Unsubscribe
`
	return os.WriteFile(path, []byte(content), 0644)
}
