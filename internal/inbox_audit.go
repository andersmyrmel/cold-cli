package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type AuditInboxHistoryConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	Since          time.Time
	SecretResolver SecretResolver
	GWS            GWSClient
	IMAP           IMAPMessageLister
}

type InboxAuditMessage struct {
	CampaignID   int64     `json:"campaign_id"`
	LeadID       int64     `json:"lead_id"`
	AccountID    int64     `json:"account_id"`
	AccountEmail string    `json:"account_email"`
	Provider     string    `json:"provider"`
	Direction    string    `json:"direction"`
	Type         string    `json:"type"`
	MessageID    string    `json:"message_id"`
	ThreadID     string    `json:"thread_id"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Subject      string    `json:"subject"`
	OccurredAt   time.Time `json:"occurred_at"`
}

type InboxAuditAccountResult struct {
	AccountID    int64  `json:"account_id"`
	AccountEmail string `json:"account_email"`
	Provider     string `json:"provider"`
	Scanned      int    `json:"scanned"`
	Matched      int    `json:"matched"`
	Missing      int    `json:"missing"`
	Error        string `json:"error,omitempty"`
}

type InboxAuditResult struct {
	WorkspaceID string                    `json:"workspace_id"`
	Since       time.Time                 `json:"since"`
	Scanned     int                       `json:"scanned"`
	Matched     int                       `json:"matched"`
	Missing     int                       `json:"missing"`
	Accounts    []InboxAuditAccountResult `json:"accounts"`
	Messages    []InboxAuditMessage       `json:"messages"`
}

func AuditInboxHistory(cfg AuditInboxHistoryConfig) (*InboxAuditResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	workspaceID := NormalizeWorkspaceID(cfg.WorkspaceID)
	if cfg.Since.IsZero() {
		cfg.Since = time.Now().UTC().AddDate(0, 0, -120)
	}
	accounts, err := loadActiveAccounts(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("loading active accounts: %w", err)
	}
	result := &InboxAuditResult{WorkspaceID: workspaceID, Since: cfg.Since.UTC()}
	var errs []error
	for _, account := range accounts {
		if NormalizeWorkspaceID(account.WorkspaceID) != workspaceID {
			continue
		}
		accountResult := InboxAuditAccountResult{AccountID: account.ID, AccountEmail: account.Email, Provider: account.Provider}
		messages, listErr := listHistoricalProviderMessages(cfg, account)
		if listErr != nil {
			accountResult.Error = listErr.Error()
			result.Accounts = append(result.Accounts, accountResult)
			errs = append(errs, fmt.Errorf("auditing %s: %w", account.Email, listErr))
			continue
		}
		accountResult.Scanned = len(messages)
		result.Scanned += len(messages)
		sort.SliceStable(messages, func(i, j int) bool {
			return inboundEmailOccurredAt(messages[i]).Before(inboundEmailOccurredAt(messages[j]))
		})
		discoveredTargets := map[string]auditThreadTarget{}
		for _, msg := range messages {
			target, ok, matchErr := findAuditThreadTarget(cfg.DB, account, msg)
			if matchErr != nil {
				errs = append(errs, fmt.Errorf("matching %s from %s: %w", msg.ID, account.Email, matchErr))
				continue
			}
			if !ok {
				target, ok = findDiscoveredAuditThreadTarget(discoveredTargets, msg)
			}
			if !ok {
				continue
			}
			rememberDiscoveredAuditTarget(discoveredTargets, target, msg)
			accountResult.Matched++
			result.Matched++
			if auditMessageAlreadyStored(cfg.DB, target.CampaignID, target.LeadID, msg) {
				continue
			}
			missing := auditInboxMessage(target, account, msg)
			result.Messages = append(result.Messages, missing)
			accountResult.Missing++
			result.Missing++
		}
		result.Accounts = append(result.Accounts, accountResult)
	}
	sort.Slice(result.Messages, func(i, j int) bool { return result.Messages[i].OccurredAt.Before(result.Messages[j].OccurredAt) })
	return result, errors.Join(errs...)
}

func findDiscoveredAuditThreadTarget(discovered map[string]auditThreadTarget, msg GWSMessage) (auditThreadTarget, bool) {
	for _, candidate := range providerMessageIDCandidates(msg) {
		if target, ok := discovered[canonicalMessageID(candidate)]; ok {
			return target, true
		}
	}
	return auditThreadTarget{}, false
}

func rememberDiscoveredAuditTarget(discovered map[string]auditThreadTarget, target auditThreadTarget, msg GWSMessage) {
	for _, candidate := range providerMessageIDCandidates(msg) {
		if key := canonicalMessageID(candidate); key != "" {
			discovered[key] = target
		}
	}
}

func listHistoricalProviderMessages(cfg AuditInboxHistoryConfig, account Account) ([]GWSMessage, error) {
	switch account.Provider {
	case AccountProviderGWS:
		if cfg.GWS == nil {
			return nil, fmt.Errorf("gws client is required")
		}
		threadIDs, err := loadGWSAuditThreadIDs(cfg.DB, cfg.WorkspaceID, account.ID)
		if err != nil {
			return nil, err
		}
		var all []GWSMessage
		for _, threadID := range threadIDs {
			messages, err := cfg.GWS.GetThreadMessages(account.Email, threadID)
			if err != nil {
				return nil, fmt.Errorf("fetching Gmail thread %s: %w", threadID, err)
			}
			for _, msg := range messages {
				if !inboundEmailOccurredAt(msg).Before(cfg.Since) {
					all = append(all, msg)
				}
			}
		}
		return dedupeMailboxMessages(all), nil
	case AccountProviderSMTPIMAP:
		lister := cfg.IMAP
		if lister == nil {
			transport := NewIMAPTransport(cfg.SecretResolver)
			transport.SearchRateLimitRetries = 2
			lister = transport
		}
		threadLister, ok := lister.(IMAPThreadMessageLister)
		if !ok {
			return nil, fmt.Errorf("IMAP transport does not support historical thread search")
		}
		anchors, err := loadIMAPAuditAnchors(cfg.DB, cfg.WorkspaceID, account.ID)
		if err != nil {
			return nil, err
		}
		return listIMAPAuditThreadMessages(threadLister, account, cfg.Since, anchors)
	default:
		return nil, fmt.Errorf("unsupported provider %q", account.Provider)
	}
}

func loadGWSAuditThreadIDs(db *sql.DB, workspaceID string, accountID int64) ([]string, error) {
	rows, err := queryDB(db, `SELECT DISTINCT e.thread_id
		FROM events e JOIN campaigns c ON c.id = e.campaign_id
		WHERE c.workspace_id = ? AND e.account_id = ?
		AND e.type IN ('sent', 'manual_reply') AND e.thread_id <> ''
		ORDER BY e.thread_id`, NormalizeWorkspaceID(workspaceID), accountID)
	if err != nil {
		return nil, fmt.Errorf("loading Gmail campaign threads: %w", err)
	}
	defer rows.Close()
	var threadIDs []string
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return nil, err
		}
		threadIDs = append(threadIDs, threadID)
	}
	return threadIDs, rows.Err()
}

const imapAuditAnchorBatchSize = 5

func loadIMAPAuditAnchors(db *sql.DB, workspaceID string, accountID int64) ([]string, error) {
	rows, err := queryDB(db, `SELECT DISTINCT e.message_id
		FROM events e JOIN campaigns c ON c.id = e.campaign_id
		WHERE c.workspace_id = ? AND e.account_id = ?
		AND e.type IN ('sent', 'manual_reply') AND e.message_id <> ''
		ORDER BY e.message_id`, NormalizeWorkspaceID(workspaceID), accountID)
	if err != nil {
		return nil, fmt.Errorf("loading campaign message anchors: %w", err)
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var anchor string
		if err := rows.Scan(&anchor); err != nil {
			return nil, err
		}
		if looksLikeMessageID(normalizeMessageID(anchor)) {
			anchors = append(anchors, normalizeMessageID(anchor))
		}
	}
	return anchors, rows.Err()
}

func listIMAPAuditThreadMessages(lister IMAPThreadMessageLister, account Account, since time.Time, initialAnchors []string) ([]GWSMessage, error) {
	knownAnchors := map[string]struct{}{}
	var pending []string
	for _, anchor := range initialAnchors {
		key := canonicalMessageID(anchor)
		if key == "" {
			continue
		}
		if _, exists := knownAnchors[key]; exists {
			continue
		}
		knownAnchors[key] = struct{}{}
		pending = append(pending, anchor)
	}
	var all []GWSMessage
	seenMessages := map[string]struct{}{}
	for round := 0; round < 4 && len(pending) > 0; round++ {
		current := pending
		pending = nil
		for start := 0; start < len(current); start += imapAuditAnchorBatchSize {
			end := start + imapAuditAnchorBatchSize
			if end > len(current) {
				end = len(current)
			}
			messages, err := lister.ListThreadMessages(account, since, current[start:end])
			if err != nil {
				return nil, fmt.Errorf("searching campaign message anchors %d-%d: %w", start+1, end, err)
			}
			for _, msg := range messages {
				messageKey := canonicalMessageID(msg.ID)
				if messageKey != "" {
					if _, exists := seenMessages[messageKey]; exists {
						continue
					}
					seenMessages[messageKey] = struct{}{}
				}
				all = append(all, msg)
				for _, candidate := range providerMessageIDCandidates(msg) {
					key := canonicalMessageID(candidate)
					if key == "" {
						continue
					}
					if _, exists := knownAnchors[key]; !exists {
						knownAnchors[key] = struct{}{}
						pending = append(pending, candidate)
					}
				}
			}
		}
	}
	return dedupeMailboxMessages(all), nil
}

func providerMessageIDCandidates(msg GWSMessage) []string {
	candidates := []string{msg.ID, msg.InReplyTo}
	if msg.Headers != nil {
		candidates = append(candidates, firstEmailHeader(msg.Headers, "Message-ID"))
		candidates = append(candidates, messageIDs(firstEmailHeader(msg.Headers, "References"))...)
	}
	return candidates
}

type auditThreadTarget struct {
	CampaignID int64
	LeadID     int64
	AccountID  int64
	ThreadID   string
}

type auditMatchCandidate struct {
	column      string
	value       string
	sameAccount bool
}

func findAuditThreadTarget(db *sql.DB, account Account, msg GWSMessage) (auditThreadTarget, bool, error) {
	candidates := []auditMatchCandidate{
		{column: "message_id", value: msg.InReplyTo},
		{column: "thread_id", value: msg.ThreadID, sameAccount: true},
	}
	if msg.Headers != nil {
		for _, reference := range messageIDs(firstEmailHeader(msg.Headers, "References")) {
			candidates = append(candidates, auditMatchCandidate{column: "message_id", value: reference})
		}
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.value) == "" {
			continue
		}
		query := fmt.Sprintf(`SELECT campaign_id, lead_id, account_id, thread_id
			FROM events WHERE %s = ? AND type IN ('sent', 'manual_reply')`, candidate.column)
		args := []any{candidate.value}
		if candidate.sameAccount {
			query += " AND account_id = ?"
			args = append(args, account.ID)
		}
		query += " ORDER BY timestamp DESC, id DESC LIMIT 1"
		var target auditThreadTarget
		err := queryRowDB(db, query, args...).Scan(&target.CampaignID, &target.LeadID, &target.AccountID, &target.ThreadID)
		if err == nil {
			return target, true, nil
		}
		if err != sql.ErrNoRows {
			return auditThreadTarget{}, false, err
		}
	}
	return auditThreadTarget{}, false, nil
}

func auditMessageAlreadyStored(db *sql.DB, campaignID, leadID int64, msg GWSMessage) bool {
	if existing, err := findEmailMessageSnapshotForProviderMessage(db, campaignID, leadID, msg); err == nil && existing != nil {
		return true
	}
	var count int
	if err := queryRowDB(db, `SELECT COUNT(*) FROM events
		WHERE campaign_id = ? AND lead_id = ? AND message_id = ?`, campaignID, leadID, msg.ID).Scan(&count); err == nil && count > 0 {
		return true
	}
	return false
}

func auditInboxMessage(target auditThreadTarget, account Account, msg GWSMessage) InboxAuditMessage {
	direction := EmailMessageDirectionInbound
	messageType := EmailMessageTypeReply
	if sameEmailAddress(msg.From, account.Email) {
		direction = EmailMessageDirectionOutbound
		messageType = EmailMessageTypeManualReply
	} else {
		switch classifyInboundMessage(msg) {
		case inboundClassificationUnsubscribe:
			messageType = EmailMessageTypeUnsubscribe
		case inboundClassificationBounce:
			messageType = EmailMessageTypeBounce
		case inboundClassificationAutoReply:
			messageType = EmailMessageTypeAutoReply
		}
	}
	return InboxAuditMessage{
		CampaignID: target.CampaignID, LeadID: target.LeadID, AccountID: account.ID,
		AccountEmail: account.Email, Provider: account.Provider, Direction: direction, Type: messageType,
		MessageID: msg.ID, ThreadID: target.ThreadID, From: msg.From, To: msg.To,
		Subject: msg.Subject, OccurredAt: inboundEmailOccurredAt(msg),
	}
}
