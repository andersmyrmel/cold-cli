package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SyncEmailThreadConfig refreshes one campaign/lead conversation from its
// provider mailbox before an operator reviews or replies to it.
type SyncEmailThreadConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	CampaignID     int64
	LeadID         int64
	ThreadID       string
	DryRun         bool
	SecretResolver SecretResolver
	GWS            GWSClient
	IMAP           IMAPMessageLister
}

type SyncEmailThreadResult struct {
	CampaignID    int64  `json:"campaign_id"`
	LeadID        int64  `json:"lead_id"`
	AccountID     int64  `json:"account_id"`
	AccountEmail  string `json:"account_email"`
	Provider      string `json:"provider"`
	ThreadID      string `json:"thread_id"`
	Fetched       int    `json:"fetched"`
	Matched       int    `json:"matched"`
	Added         int    `json:"added"`
	InboundAdded  int    `json:"inbound_added"`
	OutboundAdded int    `json:"outbound_added"`
	Stored        int    `json:"stored"`
	DryRun        bool   `json:"dry_run"`
}

type emailThreadSyncTarget struct {
	CampaignID int64
	LeadID     int64
	ThreadID   string
	Account    Account
	Since      time.Time
}

func SyncEmailThread(cfg SyncEmailThreadConfig) (*SyncEmailThreadResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	if cfg.CampaignID == 0 {
		return nil, errRequired("campaign_id")
	}
	if cfg.LeadID == 0 {
		return nil, errRequired("lead_id")
	}

	target, err := resolveEmailThreadSyncTarget(cfg.DB, cfg.WorkspaceID, cfg.CampaignID, cfg.LeadID, cfg.ThreadID)
	if err != nil {
		return nil, err
	}

	var providerMessages []GWSMessage
	switch target.Account.Provider {
	case AccountProviderGWS:
		if cfg.GWS == nil {
			return nil, fmt.Errorf("gws client is required to refresh %s", target.Account.Email)
		}
		providerMessages, err = cfg.GWS.GetThreadMessages(target.Account.Email, target.ThreadID)
		if err != nil {
			return nil, fmt.Errorf("refreshing Gmail thread %s: %w", target.ThreadID, err)
		}
	case AccountProviderSMTPIMAP:
		imapLister := cfg.IMAP
		if imapLister == nil {
			transport := NewIMAPTransport(cfg.SecretResolver)
			transport.SentMailboxes = []string{"Sent", "Sent Items", "Sent Messages"}
			imapLister = transport
		}
		knownIDs, knownErr := loadKnownThreadMessageIDs(cfg.DB, target)
		if knownErr != nil {
			return nil, knownErr
		}
		if threadLister, ok := imapLister.(IMAPThreadMessageLister); ok {
			providerMessages, err = threadLister.ListThreadMessages(target.Account, target.Since, sortedKnownMessageIDs(knownIDs))
		} else {
			providerMessages, err = imapLister.ListMessages(target.Account, target.Since, false)
		}
		if err != nil {
			return nil, fmt.Errorf("refreshing IMAP thread %s: %w", target.ThreadID, err)
		}
	default:
		return nil, fmt.Errorf("unsupported account provider %q", target.Account.Provider)
	}

	knownIDs, err := loadKnownThreadMessageIDs(cfg.DB, target)
	if err != nil {
		return nil, err
	}
	matched := providerMessages
	if target.Account.Provider == AccountProviderSMTPIMAP {
		matched = filterIMAPThreadMessages(providerMessages, target.ThreadID, knownIDs)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Date.Equal(matched[j].Date) {
			return matched[i].ID < matched[j].ID
		}
		return matched[i].Date.Before(matched[j].Date)
	})

	result := &SyncEmailThreadResult{
		CampaignID: target.CampaignID, LeadID: target.LeadID,
		AccountID: target.Account.ID, AccountEmail: target.Account.Email,
		Provider: target.Account.Provider, ThreadID: target.ThreadID,
		Fetched: len(providerMessages), Matched: len(matched), DryRun: cfg.DryRun,
	}

	baseEvent := emailMessageBackfillEvent{
		CampaignID:   target.CampaignID,
		LeadID:       target.LeadID,
		AccountID:    target.Account.ID,
		AccountEmail: target.Account.Email,
		Provider:     target.Account.Provider,
		ThreadID:     target.ThreadID,
		Timestamp:    target.Since,
	}
	for _, msg := range matched {
		if shouldSkipProviderThreadMessage(msg, target.Account.Email, time.Now().UTC()) {
			continue
		}
		exists, err := emailMessageSnapshotExistsForGWSMessage(cfg.DB, target.CampaignID, target.LeadID, msg)
		if err != nil {
			return nil, fmt.Errorf("checking stored thread message %s: %w", msg.ID, err)
		}
		if exists {
			continue
		}

		direction := EmailMessageDirectionInbound
		messageType := EmailMessageTypeReply
		if sameEmailAddress(msg.From, target.Account.Email) {
			direction = EmailMessageDirectionOutbound
			messageType = EmailMessageTypeManualReply
		} else {
			classificationMessage := msg
			if strings.TrimSpace(classificationMessage.Snippet) == "" {
				classificationMessage.Snippet = classificationMessage.TextBody
			}
			switch classifyInboundMessage(classificationMessage) {
			case inboundClassificationUnsubscribe:
				messageType = EmailMessageTypeUnsubscribe
			case inboundClassificationBounce:
				messageType = EmailMessageTypeBounce
			case inboundClassificationAutoReply:
				messageType = EmailMessageTypeAutoReply
			}
		}
		result.Added++
		if direction == EmailMessageDirectionOutbound {
			result.OutboundAdded++
		} else {
			result.InboundAdded++
		}
		if cfg.DryRun {
			continue
		}

		stored := emailMessageFromThreadMessage(baseEvent, msg, direction, messageType)
		stored.ThreadID = target.ThreadID
		if err := insertEmailMessage(cfg.DB, stored); err != nil {
			return nil, fmt.Errorf("storing refreshed thread message %s: %w", msg.ID, err)
		}
	}

	if err := queryRowDB(cfg.DB, `SELECT COUNT(*) FROM email_messages
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ?`,
		target.CampaignID, target.LeadID, target.ThreadID).Scan(&result.Stored); err != nil {
		return nil, fmt.Errorf("counting stored thread messages: %w", err)
	}
	if cfg.DryRun {
		result.Stored += result.Added
	}
	return result, nil
}

func resolveEmailThreadSyncTarget(db *sql.DB, workspaceID string, campaignID, leadID int64, requestedThreadID string) (emailThreadSyncTarget, error) {
	var campaignWorkspace string
	if err := queryRowDB(db, `SELECT workspace_id FROM campaigns WHERE id = ?`, campaignID).Scan(&campaignWorkspace); err != nil {
		if err == sql.ErrNoRows {
			return emailThreadSyncTarget{}, fmt.Errorf("campaign %d not found", campaignID)
		}
		return emailThreadSyncTarget{}, fmt.Errorf("loading campaign: %w", err)
	}
	if workspace := strings.TrimSpace(workspaceID); workspace != "" && campaignWorkspace != NormalizeWorkspaceID(workspace) {
		return emailThreadSyncTarget{}, fmt.Errorf("campaign %d is not in workspace %s", campaignID, NormalizeWorkspaceID(workspace))
	}

	var member int
	if err := queryRowDB(db, `SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ? AND lead_id = ?`, campaignID, leadID).Scan(&member); err != nil {
		return emailThreadSyncTarget{}, fmt.Errorf("checking campaign lead: %w", err)
	}
	if member == 0 {
		return emailThreadSyncTarget{}, fmt.Errorf("lead %d is not in campaign %d", leadID, campaignID)
	}

	threadID := strings.TrimSpace(requestedThreadID)
	if threadID == "" {
		err := queryRowDB(db, `SELECT thread_id FROM email_messages
			WHERE campaign_id = ? AND lead_id = ? AND thread_id <> ''
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, campaignID, leadID).Scan(&threadID)
		if err == sql.ErrNoRows {
			err = queryRowDB(db, `SELECT thread_id FROM events
				WHERE campaign_id = ? AND lead_id = ? AND thread_id <> ''
				ORDER BY timestamp DESC, id DESC LIMIT 1`, campaignID, leadID).Scan(&threadID)
		}
		if err == sql.ErrNoRows {
			return emailThreadSyncTarget{}, fmt.Errorf("no stored thread found for campaign %d lead %d", campaignID, leadID)
		}
		if err != nil {
			return emailThreadSyncTarget{}, fmt.Errorf("resolving latest thread: %w", err)
		}
	}

	var accountID int64
	err := queryRowDB(db, `SELECT account_id FROM email_messages
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ? AND direction = 'outbound'
		ORDER BY occurred_at ASC, id ASC LIMIT 1`, campaignID, leadID, threadID).Scan(&accountID)
	if err == sql.ErrNoRows {
		err = queryRowDB(db, `SELECT account_id FROM events
			WHERE campaign_id = ? AND lead_id = ? AND thread_id = ? AND type IN ('sent', 'manual_reply')
			ORDER BY timestamp ASC, id ASC LIMIT 1`, campaignID, leadID, threadID).Scan(&accountID)
	}
	if err == sql.ErrNoRows {
		err = queryRowDB(db, `SELECT account_id FROM email_messages
			WHERE campaign_id = ? AND lead_id = ? AND thread_id = ?
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, campaignID, leadID, threadID).Scan(&accountID)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return emailThreadSyncTarget{}, fmt.Errorf("thread %s has no owning account", threadID)
		}
		return emailThreadSyncTarget{}, fmt.Errorf("resolving thread account: %w", err)
	}

	account, err := getAccountByID(db, accountID)
	if err != nil {
		return emailThreadSyncTarget{}, err
	}
	if account.WorkspaceID != campaignWorkspace {
		return emailThreadSyncTarget{}, fmt.Errorf("thread account %s is not in campaign workspace", account.Email)
	}
	if account.Status != "active" {
		return emailThreadSyncTarget{}, fmt.Errorf("account %s is %s", account.Email, account.Status)
	}

	var messageSince, eventSince time.Time
	messageSinceErr := queryRowDB(db, `SELECT occurred_at FROM email_messages
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ?
		ORDER BY occurred_at ASC, id ASC LIMIT 1`, campaignID, leadID, threadID).Scan(&messageSince)
	if messageSinceErr != nil && messageSinceErr != sql.ErrNoRows {
		return emailThreadSyncTarget{}, fmt.Errorf("loading thread start: %w", messageSinceErr)
	}
	eventSinceErr := queryRowDB(db, `SELECT timestamp FROM events
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ?
		ORDER BY timestamp ASC, id ASC LIMIT 1`, campaignID, leadID, threadID).Scan(&eventSince)
	if eventSinceErr != nil && eventSinceErr != sql.ErrNoRows {
		return emailThreadSyncTarget{}, fmt.Errorf("loading event thread start: %w", eventSinceErr)
	}
	since := time.Now().UTC().AddDate(-1, 0, 0)
	if messageSinceErr == nil {
		since = messageSince.UTC()
	}
	if eventSinceErr == nil && (messageSinceErr != nil || eventSince.Before(since)) {
		since = eventSince.UTC()
	}

	return emailThreadSyncTarget{
		CampaignID: campaignID, LeadID: leadID, ThreadID: threadID,
		Account: account, Since: since,
	}, nil
}

func loadKnownThreadMessageIDs(db *sql.DB, target emailThreadSyncTarget) (map[string]struct{}, error) {
	known := map[string]struct{}{}
	addKnownMessageID(known, target.ThreadID)

	rows, err := queryDB(db, `SELECT message_id, raw_headers FROM email_messages
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ?`,
		target.CampaignID, target.LeadID, target.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("loading stored thread identifiers: %w", err)
	}
	for rows.Next() {
		var messageID, rawHeaders string
		if err := rows.Scan(&messageID, &rawHeaders); err != nil {
			rows.Close()
			return nil, err
		}
		addKnownMessageID(known, messageID)
		var headers map[string]string
		if json.Unmarshal([]byte(rawHeaders), &headers) == nil {
			addKnownMessageID(known, firstEmailHeader(headers, "Message-ID"))
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = queryDB(db, `SELECT message_id FROM events
		WHERE campaign_id = ? AND lead_id = ? AND thread_id = ? AND message_id <> ''`,
		target.CampaignID, target.LeadID, target.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("loading event thread identifiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, err
		}
		addKnownMessageID(known, messageID)
	}
	return known, rows.Err()
}

func filterIMAPThreadMessages(messages []GWSMessage, threadID string, known map[string]struct{}) []GWSMessage {
	remaining := append([]GWSMessage{}, messages...)
	matched := make([]GWSMessage, 0, len(messages))
	for {
		added := false
		next := remaining[:0]
		for _, msg := range remaining {
			if imapMessageBelongsToThread(msg, threadID, known) {
				matched = append(matched, msg)
				addKnownMessageID(known, msg.ID)
				if msg.Headers != nil {
					addKnownMessageID(known, firstEmailHeader(msg.Headers, "Message-ID"))
				}
				added = true
				continue
			}
			next = append(next, msg)
		}
		remaining = next
		if !added {
			break
		}
	}
	return matched
}

func imapMessageBelongsToThread(msg GWSMessage, threadID string, known map[string]struct{}) bool {
	for _, candidate := range []string{msg.ID, msg.ThreadID, msg.InReplyTo} {
		if knownMessageID(known, candidate) {
			return true
		}
	}
	if knownMessageID(known, threadID) && sameMessageID(msg.ThreadID, threadID) {
		return true
	}
	if msg.Headers != nil {
		if knownMessageID(known, firstEmailHeader(msg.Headers, "Message-ID")) {
			return true
		}
		for _, reference := range messageIDs(firstEmailHeader(msg.Headers, "References")) {
			if knownMessageID(known, reference) {
				return true
			}
		}
	}
	return false
}

func addKnownMessageID(known map[string]struct{}, value string) {
	value = canonicalMessageID(value)
	if value != "" {
		known[value] = struct{}{}
	}
}

func knownMessageID(known map[string]struct{}, value string) bool {
	_, ok := known[canonicalMessageID(value)]
	return ok && canonicalMessageID(value) != ""
}

func sameMessageID(a, b string) bool {
	return canonicalMessageID(a) != "" && canonicalMessageID(a) == canonicalMessageID(b)
}

func canonicalMessageID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sortedKnownMessageIDs(known map[string]struct{}) []string {
	values := make([]string, 0, len(known))
	for value := range known {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
