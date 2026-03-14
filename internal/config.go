package internal

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultTimezone   string `yaml:"default_timezone"`
	DefaultDailyLimit int    `yaml:"default_daily_limit"`
	MinGapSeconds     int    `yaml:"min_gap_seconds"`
	MaxGapSeconds     int    `yaml:"max_gap_seconds"`
	SendWindowStart   string `yaml:"send_window_start"`
	SendWindowEnd     string `yaml:"send_window_end"`
	SendDays          string `yaml:"send_days"`
}

var DefaultConfig = Config{
	DefaultTimezone:   "America/New_York",
	DefaultDailyLimit: 50,
	MinGapSeconds:     90,
	MaxGapSeconds:     140,
	SendWindowStart:   "09:00",
	SendWindowEnd:     "17:00",
	SendDays:          "1,2,3,4,5",
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
	data, err := yaml.Marshal(DefaultConfig)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
