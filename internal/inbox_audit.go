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
		for _, msg := range messages {
			target, ok, matchErr := findAuditThreadTarget(cfg.DB, account, msg)
			if matchErr != nil {
				errs = append(errs, fmt.Errorf("matching %s from %s: %w", msg.ID, account.Email, matchErr))
				continue
			}
			if !ok {
				continue
			}
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

func listHistoricalProviderMessages(cfg AuditInboxHistoryConfig, account Account) ([]GWSMessage, error) {
	switch account.Provider {
	case AccountProviderGWS:
		if cfg.GWS == nil {
			return nil, fmt.Errorf("gws client is required")
		}
		query := fmt.Sprintf("after:%d", cfg.Since.Unix())
		return cfg.GWS.ListMessages(account.Email, query, true)
	case AccountProviderSMTPIMAP:
		lister := cfg.IMAP
		if lister == nil {
			lister = NewIMAPTransport(cfg.SecretResolver)
		}
		if historical, ok := lister.(IMAPHistoryMessageLister); ok {
			return historical.ListAllMessages(account, cfg.Since)
		}
		return lister.ListMessages(account, cfg.Since, true)
	default:
		return nil, fmt.Errorf("unsupported provider %q", account.Provider)
	}
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
