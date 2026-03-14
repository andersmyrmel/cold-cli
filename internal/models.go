package internal

import "time"

type Account struct {
	ID         int64     `json:"id"`
	Email      string    `json:"email"`
	DailyLimit int       `json:"daily_limit"`
	LastSendAt *time.Time `json:"last_send_at,omitempty"`
	Status     string    `json:"status"`
}

type Campaign struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Status            string `json:"status"`
	SequenceFile      string `json:"sequence_file"`
	StopOnReply       bool   `json:"stop_on_reply"`
	StopOnDomainReply bool   `json:"stop_on_domain_reply"`
	SendWindowStart   string `json:"send_window_start"`
	SendWindowEnd     string `json:"send_window_end"`
	SendDays          string `json:"send_days"`
	Timezone          string `json:"timezone"`
	MinGapSeconds     int    `json:"min_gap_seconds"`
	MaxGapSeconds     int    `json:"max_gap_seconds"`
	CreatedAt         time.Time `json:"created_at"`
}

type Lead struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Company      string `json:"company"`
	Domain       string `json:"domain"`
	CustomFields string `json:"custom_fields,omitempty"`
	GlobalStatus string `json:"global_status"`
	CreatedAt    time.Time `json:"created_at"`
}

type CampaignLead struct {
	CampaignID int64     `json:"campaign_id"`
	LeadID     int64     `json:"lead_id"`
	Status     string    `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
}

type ScheduledSend struct {
	ID              int64      `json:"id"`
	CampaignID      int64      `json:"campaign_id"`
	LeadID          int64      `json:"lead_id"`
	AccountID       int64      `json:"account_id"`
	StepNumber      int        `json:"step_number"`
	VariantIndex    int        `json:"variant_index"`
	SendAt          time.Time  `json:"send_at"`
	Status          string     `json:"status"`
	ThreadID        string     `json:"thread_id,omitempty"`
	ParentMessageID string     `json:"parent_message_id,omitempty"`
	MessageID       string     `json:"message_id,omitempty"`
	SentAt          *time.Time `json:"sent_at,omitempty"`
}

type Event struct {
	ID         int64     `json:"id"`
	CampaignID int64     `json:"campaign_id"`
	LeadID     int64     `json:"lead_id"`
	AccountID  int64     `json:"account_id"`
	Type       string    `json:"type"`
	StepNumber int       `json:"step_number"`
	MessageID  string    `json:"message_id"`
	ThreadID   string    `json:"thread_id"`
	Timestamp  time.Time `json:"timestamp"`
	Metadata   string    `json:"metadata,omitempty"`
}
