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
	discordIdleReminderInterval = 24 * time.Hour

	DiscordEventCampaignCompleted             = "campaign_completed"
	DiscordEventCampaignCompletedWithFailures = "campaign_completed_with_failures"
	DiscordEventSenderIdle                    = "sender_idle"
	DiscordEventInboxReconciliationFailed     = "inbox_reconciliation_failed"
)

// DiscordNotifier sends a single cold-cli event to Discord.
type DiscordNotifier interface {
	NotifyDiscord(context.Context, DiscordNotificationEvent) error
}

// DiscordNotifyOptions configures one notification processing pass.
type DiscordNotifyOptions struct {
	Limit     int
	Providers []string
}

// DiscordOperationalNotifyOptions configures campaign completion and sender-idle alerts.
// Operational alerts are disabled when Workspaces is empty.
type DiscordOperationalNotifyOptions struct {
	Workspaces           []string
	Now                  time.Time
	IdleReminderInterval time.Duration
}

// DiscordNotificationEvent is the compact event shape used for Discord alerts.
type DiscordNotificationEvent struct {
	EventID           int64
	EventType         string
	Timestamp         string
	WorkspaceID       string
	CampaignID        int64
	CampaignName      string
	CampaignStatus    string
	LeadEmail         string
	LeadCompany       string
	AccountEmail      string
	AccountEmails     []string
	IdleAccountEmails []string
	FromEmail         string
	Subject           string
	Snippet           string
	MessageID         string
	LeadsContacted    int
	SentCount         int
	ReplyCount        int
	UnsubscribeCount  int
	BounceCount       int
	FailedCount       int
	SkippedCount      int
	CancelledCount    int
	PendingCount      int
	IdleSince         string
	Reminder          bool
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
	switch event.EventType {
	case DiscordEventCampaignCompleted, DiscordEventCampaignCompletedWithFailures:
		return buildDiscordCampaignPayload(event)
	case DiscordEventSenderIdle:
		return buildDiscordSenderIdlePayload(event)
	case DiscordEventInboxReconciliationFailed:
		return buildDiscordInboxReconciliationFailurePayload(event)
	}

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

func buildDiscordInboxReconciliationFailurePayload(event DiscordNotificationEvent) discordWebhookPayload {
	workspace := discordWorkspaceLabel(event.WorkspaceID)
	description := truncateDiscordText(cleanDiscordText(event.Snippet), 1000)
	if description == "" {
		description = "The provider audit did not reach a verified clean state. Follow-up selection must remain blocked."
	}
	return discordWebhookPayload{
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
		Embeds: []discordEmbed{{
			Title:       workspace + " inbox reconciliation failed",
			Description: description,
			Color:       0xef4444,
			Timestamp:   event.Timestamp,
			Fields: []discordEmbedField{
				{Name: "Workspace", Value: discordFieldValue(event.WorkspaceID), Inline: true},
			},
		}},
	}
}

func buildDiscordCampaignPayload(event DiscordNotificationEvent) discordWebhookPayload {
	workspace := discordWorkspaceLabel(event.WorkspaceID)
	title := workspace + " campaign finished"
	color := 0x22c55e
	if event.EventType == DiscordEventCampaignCompletedWithFailures || event.CampaignStatus == CampaignStatusCompletedWithFailures {
		title = workspace + " campaign finished with failures"
		color = 0xef4444
	}

	description := "All scheduled sends reached a terminal state."
	if len(event.IdleAccountEmails) > 0 {
		description = fmt.Sprintf("%d active inbox(es) now have no pending sends. Prepare or activate the next campaign.", len(event.IdleAccountEmails))
	} else if len(event.AccountEmails) > 0 {
		description += " Its inboxes still have pending sends in another active campaign."
	}

	fields := []discordEmbedField{
		{Name: "Workspace", Value: discordFieldValue(event.WorkspaceID), Inline: true},
		{Name: "Campaign", Value: discordFieldValue(event.CampaignName), Inline: true},
		{Name: "Result", Value: discordFieldValue(event.CampaignStatus), Inline: true},
		{Name: "Delivery", Value: discordFieldValue(fmt.Sprintf("%d leads contacted · %d emails sent", event.LeadsContacted, event.SentCount)), Inline: false},
		{Name: "Responses", Value: discordFieldValue(fmt.Sprintf("%d replies · %d unsubscribes · %d bounces", event.ReplyCount, event.UnsubscribeCount, event.BounceCount)), Inline: false},
		{Name: "Other outcomes", Value: discordFieldValue(fmt.Sprintf("%d failed · %d skipped · %d cancelled", event.FailedCount, event.SkippedCount, event.CancelledCount)), Inline: false},
		{Name: "Inboxes", Value: discordFieldValue(strings.Join(event.AccountEmails, "\n")), Inline: false},
	}
	if len(event.IdleAccountEmails) > 0 {
		fields = append(fields, discordEmbedField{Name: "Now idle", Value: discordFieldValue(strings.Join(event.IdleAccountEmails, "\n")), Inline: false})
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

func buildDiscordSenderIdlePayload(event DiscordNotificationEvent) discordWebhookPayload {
	workspace := discordWorkspaceLabel(event.WorkspaceID)
	title := workspace + " sender is idle"
	if event.Reminder {
		title = workspace + " sender is still idle"
	}
	description := "This active sender has no pending sends in any active campaign. Prepare or activate its next campaign."
	fields := []discordEmbedField{
		{Name: "Workspace", Value: discordFieldValue(event.WorkspaceID), Inline: true},
		{Name: "Inbox", Value: discordFieldValue(event.AccountEmail), Inline: true},
		{Name: "Pending sends", Value: strconv.Itoa(event.PendingCount), Inline: true},
	}
	if event.CampaignName != "" {
		fields = append(fields, discordEmbedField{Name: "Most recent campaign", Value: discordFieldValue(event.CampaignName), Inline: false})
	}
	if event.IdleSince != "" {
		fields = append(fields, discordEmbedField{Name: "Idle since", Value: discordFieldValue(event.IdleSince), Inline: false})
	}

	return discordWebhookPayload{
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
		Embeds: []discordEmbed{{
			Title:       title,
			Description: description,
			Color:       0xf59e0b,
			Timestamp:   event.Timestamp,
			Fields:      fields,
		}},
	}
}

func discordWorkspaceLabel(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "Cold email"
	}
	if strings.EqualFold(workspace, "storeinspect") {
		return "StoreInspect"
	}
	return workspace
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

	events, err := listDiscordNotificationEvents(db, lastID, limit, opts.Providers)
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

// ProcessDiscordOperationalNotifications sends durable, workspace-scoped
// campaign completion and sender-idle alerts. Completion alerts include any
// campaign inboxes that became idle, and suppress a duplicate immediate idle
// alert for those inboxes. Idle reminders repeat at most once per interval.
func ProcessDiscordOperationalNotifications(ctx context.Context, db *sql.DB, notifier DiscordNotifier, opts DiscordOperationalNotifyOptions) (int, error) {
	if notifier == nil {
		return 0, nil
	}

	workspaces := cleanDiscordOperationalWorkspaces(opts.Workspaces)
	if len(workspaces) == 0 {
		return 0, nil
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	reminderInterval := opts.IdleReminderInterval
	if reminderInterval <= 0 {
		reminderInterval = discordIdleReminderInterval
	}

	completed, err := processDiscordCampaignCompletions(ctx, db, notifier, workspaces, now)
	if err != nil {
		return completed, err
	}
	idle, err := processDiscordIdleAccounts(ctx, db, notifier, workspaces, now, reminderInterval)
	return completed + idle, err
}

type discordCampaignCompletion struct {
	ID          int64
	WorkspaceID string
	Name        string
	Status      string
	CompletedAt string
}

type discordCampaignStats struct {
	LeadsContacted int
	Sent           int
	Replies        int
	Unsubscribes   int
	Bounces        int
	Failed         int
	Skipped        int
	Cancelled      int
}

type discordAccountRef struct {
	ID    int64
	Email string
}

func processDiscordCampaignCompletions(ctx context.Context, db *sql.DB, notifier DiscordNotifier, workspaces []string, now time.Time) (int, error) {
	campaigns, err := listUnnotifiedCampaignCompletions(db, workspaces)
	if err != nil {
		return 0, err
	}

	notified := 0
	for _, campaign := range campaigns {
		stats, err := loadDiscordCampaignStats(db, campaign.ID)
		if err != nil {
			return notified, err
		}
		accounts, err := loadDiscordCampaignAccounts(db, campaign.ID)
		if err != nil {
			return notified, err
		}
		idleAccounts, err := loadIdleDiscordCampaignAccounts(db, campaign.ID, campaign.WorkspaceID)
		if err != nil {
			return notified, err
		}

		eventType := DiscordEventCampaignCompleted
		if campaign.Status == CampaignStatusCompletedWithFailures {
			eventType = DiscordEventCampaignCompletedWithFailures
		}
		event := DiscordNotificationEvent{
			EventType:         eventType,
			Timestamp:         campaign.CompletedAt,
			WorkspaceID:       campaign.WorkspaceID,
			CampaignID:        campaign.ID,
			CampaignName:      campaign.Name,
			CampaignStatus:    campaign.Status,
			AccountEmails:     discordAccountEmails(accounts),
			IdleAccountEmails: discordAccountEmails(idleAccounts),
			LeadsContacted:    stats.LeadsContacted,
			SentCount:         stats.Sent,
			ReplyCount:        stats.Replies,
			UnsubscribeCount:  stats.Unsubscribes,
			BounceCount:       stats.Bounces,
			FailedCount:       stats.Failed,
			SkippedCount:      stats.Skipped,
			CancelledCount:    stats.Cancelled,
		}
		if err := notifier.NotifyDiscord(ctx, event); err != nil {
			return notified, err
		}
		if err := markDiscordCampaignCompletionNotified(db, campaign, idleAccounts, now); err != nil {
			return notified, err
		}
		notified++
	}
	return notified, nil
}

func listUnnotifiedCampaignCompletions(db *sql.DB, workspaces []string) ([]discordCampaignCompletion, error) {
	query := `
		SELECT id, workspace_id, name, status, completed_at
		FROM campaigns
		WHERE status IN ('completed', 'completed_with_failures')
			AND completed_at IS NOT NULL
			AND completion_notified_at IS NULL`
	args := []any{}
	query, args = appendDiscordWorkspaceFilter(query, args, "workspace_id", workspaces)
	query += " ORDER BY completed_at ASC, id ASC"

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying unnotified campaign completions: %w", err)
	}
	defer rows.Close()

	var campaigns []discordCampaignCompletion
	for rows.Next() {
		var campaign discordCampaignCompletion
		if err := rows.Scan(&campaign.ID, &campaign.WorkspaceID, &campaign.Name, &campaign.Status, &campaign.CompletedAt); err != nil {
			return nil, fmt.Errorf("scanning campaign completion: %w", err)
		}
		campaigns = append(campaigns, campaign)
	}
	return campaigns, rows.Err()
}

func loadDiscordCampaignStats(db *sql.DB, campaignID int64) (discordCampaignStats, error) {
	var stats discordCampaignStats
	if err := queryRowDB(db, `
		SELECT
			COUNT(DISTINCT CASE WHEN status = 'sent' THEN lead_id END),
			COALESCE(SUM(CASE WHEN status = 'sent' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'cancelled' THEN 1 ELSE 0 END), 0)
		FROM scheduled_sends
		WHERE campaign_id = ?`, campaignID).Scan(
		&stats.LeadsContacted,
		&stats.Sent,
		&stats.Failed,
		&stats.Skipped,
		&stats.Cancelled,
	); err != nil {
		return stats, fmt.Errorf("loading campaign delivery stats: %w", err)
	}
	if err := queryRowDB(db, `
		SELECT
			COUNT(DISTINCT CASE WHEN type = 'reply' THEN lead_id END),
			COUNT(DISTINCT CASE WHEN type = 'unsubscribe' THEN lead_id END),
			COUNT(DISTINCT CASE WHEN type = 'bounce' THEN lead_id END)
		FROM events
		WHERE campaign_id = ?`, campaignID).Scan(
		&stats.Replies,
		&stats.Unsubscribes,
		&stats.Bounces,
	); err != nil {
		return stats, fmt.Errorf("loading campaign response stats: %w", err)
	}
	return stats, nil
}

func loadDiscordCampaignAccounts(db *sql.DB, campaignID int64) ([]discordAccountRef, error) {
	rows, err := queryDB(db, `
		SELECT a.id, a.email
		FROM campaign_accounts ca
		JOIN accounts a ON a.id = ca.account_id
		WHERE ca.campaign_id = ?
		ORDER BY a.email`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("loading campaign inboxes: %w", err)
	}
	defer rows.Close()
	return scanDiscordAccountRefs(rows)
}

func loadIdleDiscordCampaignAccounts(db *sql.DB, campaignID int64, workspaceID string) ([]discordAccountRef, error) {
	rows, err := queryDB(db, `
		SELECT a.id, a.email
		FROM campaign_accounts ca
		JOIN accounts a ON a.id = ca.account_id
		WHERE ca.campaign_id = ?
			AND a.workspace_id = ?
			AND a.status = 'active'
			AND NOT EXISTS (
				SELECT 1
				FROM scheduled_sends ss
				JOIN campaigns active_campaign ON active_campaign.id = ss.campaign_id
				WHERE ss.account_id = a.id
					AND ss.status = 'pending'
					AND active_campaign.status = 'active'
			)
		ORDER BY a.email`, campaignID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("loading newly idle campaign inboxes: %w", err)
	}
	defer rows.Close()
	return scanDiscordAccountRefs(rows)
}

func scanDiscordAccountRefs(rows *sql.Rows) ([]discordAccountRef, error) {
	var accounts []discordAccountRef
	for rows.Next() {
		var account discordAccountRef
		if err := rows.Scan(&account.ID, &account.Email); err != nil {
			return nil, fmt.Errorf("scanning discord inbox: %w", err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func markDiscordCampaignCompletionNotified(db *sql.DB, campaign discordCampaignCompletion, idleAccounts []discordAccountRef, now time.Time) error {
	timestamp := now.UTC().Format(time.RFC3339)
	tx, err := beginTx(db)
	if err != nil {
		return fmt.Errorf("starting campaign notification transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE campaigns SET completion_notified_at = ?
		WHERE id = ? AND completed_at IS NOT NULL AND completion_notified_at IS NULL`, timestamp, campaign.ID); err != nil {
		return fmt.Errorf("marking campaign completion notified: %w", err)
	}
	for _, account := range idleAccounts {
		if _, err := tx.Exec(`UPDATE accounts
			SET idle_since = COALESCE(idle_since, ?), idle_notified_at = ?
			WHERE id = ?`, campaign.CompletedAt, timestamp, account.ID); err != nil {
			return fmt.Errorf("marking completed campaign inbox idle: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing campaign notification state: %w", err)
	}
	return nil
}

func processDiscordIdleAccounts(ctx context.Context, db *sql.DB, notifier DiscordNotifier, workspaces []string, now time.Time, reminderInterval time.Duration) (int, error) {
	if err := syncDiscordIdleAccountState(db, workspaces, now); err != nil {
		return 0, err
	}

	query := `
		SELECT a.id, a.workspace_id, a.email, a.idle_since, a.idle_notified_at,
			COALESCE((
				SELECT c.name
				FROM campaign_accounts ca
				JOIN campaigns c ON c.id = ca.campaign_id
				WHERE ca.account_id = a.id
				ORDER BY COALESCE(c.completed_at, c.created_at) DESC, c.id DESC
				LIMIT 1
			), '')
		FROM accounts a
		WHERE a.status = 'active'
			AND a.idle_since IS NOT NULL
			AND (a.idle_notified_at IS NULL OR a.idle_notified_at <= ?)
			AND NOT EXISTS (
				SELECT 1
				FROM scheduled_sends ss
				JOIN campaigns c ON c.id = ss.campaign_id
				WHERE ss.account_id = a.id
					AND ss.status = 'pending'
					AND c.status = 'active'
			)`
	args := []any{now.Add(-reminderInterval).UTC().Format(time.RFC3339)}
	query, args = appendDiscordWorkspaceFilter(query, args, "a.workspace_id", workspaces)
	query += " ORDER BY a.workspace_id, a.email"

	rows, err := queryDB(db, query, args...)
	if err != nil {
		return 0, fmt.Errorf("querying idle discord inboxes: %w", err)
	}
	type idleAccount struct {
		ID             int64
		WorkspaceID    string
		Email          string
		IdleSince      string
		LastNotifiedAt sql.NullString
		CampaignName   string
	}
	var accounts []idleAccount
	for rows.Next() {
		var account idleAccount
		if err := rows.Scan(&account.ID, &account.WorkspaceID, &account.Email, &account.IdleSince, &account.LastNotifiedAt, &account.CampaignName); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning idle discord inbox: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	notified := 0
	for _, account := range accounts {
		event := DiscordNotificationEvent{
			EventType:    DiscordEventSenderIdle,
			Timestamp:    now.UTC().Format(time.RFC3339),
			WorkspaceID:  account.WorkspaceID,
			CampaignName: account.CampaignName,
			AccountEmail: account.Email,
			PendingCount: 0,
			IdleSince:    account.IdleSince,
			Reminder:     account.LastNotifiedAt.Valid,
		}
		if err := notifier.NotifyDiscord(ctx, event); err != nil {
			return notified, err
		}
		if _, err := execDB(db, `UPDATE accounts SET idle_notified_at = ?
			WHERE id = ? AND idle_since IS NOT NULL`, now.UTC().Format(time.RFC3339), account.ID); err != nil {
			return notified, fmt.Errorf("marking idle inbox notified: %w", err)
		}
		notified++
	}
	return notified, nil
}

func syncDiscordIdleAccountState(db *sql.DB, workspaces []string, now time.Time) error {
	hasPending := `EXISTS (
		SELECT 1
		FROM scheduled_sends ss
		JOIN campaigns c ON c.id = ss.campaign_id
		WHERE ss.account_id = accounts.id
			AND ss.status = 'pending'
			AND c.status = 'active'
	)`

	clearQuery := `UPDATE accounts SET idle_since = NULL, idle_notified_at = NULL
		WHERE status = 'active' AND ` + hasPending
	clearArgs := []any{}
	clearQuery, clearArgs = appendDiscordWorkspaceFilter(clearQuery, clearArgs, "workspace_id", workspaces)
	if _, err := execDB(db, clearQuery, clearArgs...); err != nil {
		return fmt.Errorf("clearing resumed inbox idle state: %w", err)
	}

	idleQuery := `UPDATE accounts SET idle_since = COALESCE(idle_since, ?)
		WHERE status = 'active' AND NOT ` + hasPending
	idleArgs := []any{now.UTC().Format(time.RFC3339)}
	idleQuery, idleArgs = appendDiscordWorkspaceFilter(idleQuery, idleArgs, "workspace_id", workspaces)
	if _, err := execDB(db, idleQuery, idleArgs...); err != nil {
		return fmt.Errorf("recording inbox idle state: %w", err)
	}
	return nil
}

func cleanDiscordOperationalWorkspaces(workspaces []string) []string {
	var cleaned []string
	seen := map[string]bool{}
	for _, workspace := range workspaces {
		workspace = strings.TrimSpace(workspace)
		if workspace == "" || seen[workspace] {
			continue
		}
		seen[workspace] = true
		cleaned = append(cleaned, workspace)
	}
	return cleaned
}

func appendDiscordWorkspaceFilter(query string, args []any, column string, workspaces []string) (string, []any) {
	for _, workspace := range workspaces {
		if workspace == "*" || strings.EqualFold(workspace, "all") {
			return query, args
		}
	}
	placeholders := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		placeholders = append(placeholders, "?")
		args = append(args, workspace)
	}
	query += " AND " + column + " IN (" + strings.Join(placeholders, ",") + ")"
	return query, args
}

func discordAccountEmails(accounts []discordAccountRef) []string {
	emails := make([]string, 0, len(accounts))
	for _, account := range accounts {
		emails = append(emails, account.Email)
	}
	return emails
}

func listDiscordNotificationEvents(db *sql.DB, afterEventID int64, limit int, providers []string) ([]DiscordNotificationEvent, error) {
	query := `
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
	`
	args := []any{afterEventID}
	providers = cleanDiscordNotifyProviders(providers)
	if len(providers) > 0 {
		placeholders := make([]string, 0, len(providers))
		for _, provider := range providers {
			placeholders = append(placeholders, "?")
			args = append(args, provider)
		}
		query += " AND a.provider IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY e.id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := queryDB(db, query, args...)
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

func cleanDiscordNotifyProviders(providers []string) []string {
	var cleaned []string
	seen := map[string]bool{}
	for _, provider := range providers {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		cleaned = append(cleaned, provider)
	}
	return cleaned
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
