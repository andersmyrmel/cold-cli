package internal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	discordNotifyLastEventIDKey = "discord_notify_last_event_id"
	discordNotifyDefaultLimit   = 20
)

// DiscordNotifier sends a single cold-cli inbound event to Discord.
type DiscordNotifier interface {
	NotifyDiscord(context.Context, DiscordNotificationEvent) error
}

// DiscordNotifyOptions configures one notification processing pass.
type DiscordNotifyOptions struct {
	Limit int
}

// DiscordNotificationEvent is the compact event shape used for Discord alerts.
type DiscordNotificationEvent struct {
	EventID      int64
	EventType    string
	Timestamp    string
	CampaignName string
	LeadEmail    string
	LeadCompany  string
	AccountEmail string
	FromEmail    string
	Subject      string
	Snippet      string
	MessageID    string
}

// DiscordWebhookNotifier posts cold-cli notifications to a Discord webhook URL.
type DiscordWebhookNotifier struct {
	WebhookURL string
	Username   string
	AvatarURL  string
	HTTPClient *http.Client
}

type discordWebhookPayload struct {
	Username        string                 `json:"username,omitempty"`
	AvatarURL       string                 `json:"avatar_url,omitempty"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
	Embeds          []discordEmbed         `json:"embeds"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

func (n DiscordWebhookNotifier) NotifyDiscord(ctx context.Context, event DiscordNotificationEvent) error {
	webhookURL := strings.TrimSpace(n.WebhookURL)
	if webhookURL == "" {
		return fmt.Errorf("discord webhook URL is required")
	}

	payload := BuildDiscordWebhookPayload(event)
	payload.Username = truncateDiscordText(cleanDiscordText(n.Username), 80)
	payload.AvatarURL = strings.TrimSpace(n.AvatarURL)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding discord webhook payload: %w", err)
	}

	client := n.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating discord webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting discord webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("discord webhook returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
}

func BuildDiscordWebhookPayload(event DiscordNotificationEvent) discordWebhookPayload {
	title := "New cold email reply"
	color := 0x22c55e
	if event.EventType == EmailMessageTypeUnsubscribe || event.EventType == "unsubscribe" {
		title = "Unsubscribe request"
		color = 0xf97316
	}

	description := truncateDiscordText(cleanDiscordText(event.Snippet), 500)
	if description == "" {
		description = "No preview available."
	}

	fields := []discordEmbedField{
		{Name: "Campaign", Value: discordFieldValue(event.CampaignName), Inline: true},
		{Name: "Inbox", Value: discordFieldValue(event.AccountEmail), Inline: true},
		{Name: "Lead", Value: discordFieldValue(leadLabel(event)), Inline: false},
		{Name: "From", Value: discordFieldValue(event.FromEmail), Inline: true},
		{Name: "Subject", Value: discordFieldValue(event.Subject), Inline: false},
	}

	return discordWebhookPayload{
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
		Embeds: []discordEmbed{{
			Title:       title,
			Description: description,
			Color:       color,
			Timestamp:   event.Timestamp,
			Fields:      fields,
		}},
	}
}

// EnsureDiscordNotifyCursor initializes the Discord cursor to the current event
// high-water mark. Tick calls this before polling so first deploys do not alert
// on old historical replies, while replies found during that tick still notify.
func EnsureDiscordNotifyCursor(db *sql.DB) error {
	if _, ok, err := getKVInt64(db, discordNotifyLastEventIDKey); err != nil || ok {
		return err
	}

	var maxID int64
	if err := queryRowDB(db, "SELECT COALESCE(MAX(id), 0) FROM events").Scan(&maxID); err != nil {
		return fmt.Errorf("loading discord notification cursor baseline: %w", err)
	}
	return setKVInt64(db, discordNotifyLastEventIDKey, maxID)
}

// ProcessDiscordNotifications sends unnotified reply/unsubscribe events to Discord.
func ProcessDiscordNotifications(ctx context.Context, db *sql.DB, notifier DiscordNotifier, opts DiscordNotifyOptions) (int, error) {
	if notifier == nil {
		return 0, nil
	}

	lastID, _, err := getKVInt64(db, discordNotifyLastEventIDKey)
	if err != nil {
		return 0, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = discordNotifyDefaultLimit
	}

	events, err := listDiscordNotificationEvents(db, lastID, limit)
	if err != nil {
		return 0, err
	}

	notified := 0
	for _, event := range events {
		if err := notifier.NotifyDiscord(ctx, event); err != nil {
			return notified, err
		}
		if err := setKVInt64(db, discordNotifyLastEventIDKey, event.EventID); err != nil {
			return notified, err
		}
		notified++
	}

	return notified, nil
}

func listDiscordNotificationEvents(db *sql.DB, afterEventID int64, limit int) ([]DiscordNotificationEvent, error) {
	rows, err := queryDB(db, `
		SELECT
			e.id,
			e.type,
			e.timestamp,
			e.message_id,
			c.name,
			l.email,
			l.company,
			a.email,
			COALESCE((
				SELECT em.from_email
				FROM email_messages em
				WHERE em.message_id = e.message_id
					AND em.type = e.type
					AND em.direction = 'inbound'
				ORDER BY em.id ASC
				LIMIT 1
			), ''),
			COALESCE((
				SELECT em.subject
				FROM email_messages em
				WHERE em.message_id = e.message_id
					AND em.type = e.type
					AND em.direction = 'inbound'
				ORDER BY em.id ASC
				LIMIT 1
			), ''),
			COALESCE((
				SELECT CASE
					WHEN em.snippet <> '' THEN em.snippet
					WHEN em.display_body <> '' THEN em.display_body
					ELSE em.text_body
				END
				FROM email_messages em
				WHERE em.message_id = e.message_id
					AND em.type = e.type
					AND em.direction = 'inbound'
				ORDER BY em.id ASC
				LIMIT 1
			), '')
		FROM events e
		JOIN campaigns c ON c.id = e.campaign_id
		JOIN leads l ON l.id = e.lead_id
		JOIN accounts a ON a.id = e.account_id
		WHERE e.id > ?
			AND e.type IN ('reply', 'unsubscribe')
		ORDER BY e.id ASC
		LIMIT ?`,
		afterEventID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying discord notification events: %w", err)
	}
	defer rows.Close()

	var events []DiscordNotificationEvent
	for rows.Next() {
		var event DiscordNotificationEvent
		if err := rows.Scan(
			&event.EventID,
			&event.EventType,
			&event.Timestamp,
			&event.MessageID,
			&event.CampaignName,
			&event.LeadEmail,
			&event.LeadCompany,
			&event.AccountEmail,
			&event.FromEmail,
			&event.Subject,
			&event.Snippet,
		); err != nil {
			return nil, fmt.Errorf("scanning discord notification event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func getKVInt64(db *sql.DB, key string) (int64, bool, error) {
	var raw string
	err := queryRowDB(db, "SELECT value FROM kv WHERE key = ?", key).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("loading %s: %w", key, err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, true, fmt.Errorf("parsing %s: %w", key, err)
	}
	return value, true, nil
}

func setKVInt64(db *sql.DB, key string, value int64) error {
	raw := strconv.FormatInt(value, 10)
	if _, err := execDB(db, `INSERT INTO kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = ?`, key, raw, raw); err != nil {
		return fmt.Errorf("saving %s: %w", key, err)
	}
	return nil
}

func discordFieldValue(value string) string {
	value = truncateDiscordText(cleanDiscordText(value), 1024)
	if value == "" {
		return "-"
	}
	return value
}

func leadLabel(event DiscordNotificationEvent) string {
	if strings.TrimSpace(event.LeadCompany) == "" {
		return event.LeadEmail
	}
	return event.LeadEmail + " (" + event.LeadCompany + ")"
}

func cleanDiscordText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func truncateDiscordText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}
