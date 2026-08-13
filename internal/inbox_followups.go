package internal

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type FollowupCandidatesConfig struct {
	DB             *sql.DB
	WorkspaceID    string
	CampaignID     int64
	Since          time.Time
	Now            time.Time
	MinAge         time.Duration
	MaxFollowups   int
	Limit          int
	IncludeThread  bool
	Reconcile      bool
	SecretResolver SecretResolver
	GWS            GWSClient
	IMAP           IMAPMessageLister
}

type FindFollowupCandidatesConfig struct {
	DB            *sql.DB
	WorkspaceID   string
	CampaignID    int64
	Since         time.Time
	Now           time.Time
	MinAge        time.Duration
	MaxFollowups  int
	Limit         int
	IncludeThread bool
}

type FollowupThreadMessage struct {
	Direction  string    `json:"direction"`
	Type       string    `json:"type"`
	FromEmail  string    `json:"from_email"`
	ToEmails   string    `json:"to_emails"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	OccurredAt time.Time `json:"occurred_at"`
}

type FollowupCandidate struct {
	Rank             int                     `json:"rank"`
	CampaignID       int64                   `json:"campaign_id"`
	CampaignName     string                  `json:"campaign_name"`
	CampaignStatus   string                  `json:"campaign_status"`
	LeadID           int64                   `json:"lead_id"`
	LeadStatus       string                  `json:"lead_status"`
	LeadEmail        string                  `json:"lead_email"`
	Company          string                  `json:"company,omitempty"`
	AccountID        int64                   `json:"account_id"`
	FromEmail        string                  `json:"from_email"`
	ToEmail          string                  `json:"to_email"`
	Subject          string                  `json:"subject"`
	ThreadID         string                  `json:"thread_id"`
	ReplyCount       int                     `json:"reply_count"`
	FollowupCount    int                     `json:"followup_count"`
	MessageCount     int                     `json:"message_count"`
	AgeDays          int                     `json:"age_days"`
	LastInboundAt    time.Time               `json:"last_inbound_at"`
	LastInboundFrom  string                  `json:"last_inbound_from"`
	LastInboundBody  string                  `json:"last_inbound_body"`
	LastOutboundAt   time.Time               `json:"last_outbound_at"`
	LastOutboundBody string                  `json:"last_outbound_body"`
	Thread           []FollowupThreadMessage `json:"thread,omitempty"`
}

type FollowupCandidatesResult struct {
	WorkspaceID    string                `json:"workspace_id"`
	Since          time.Time             `json:"since"`
	MinAgeHours    int                   `json:"min_age_hours"`
	MaxFollowups   int                   `json:"max_followups"`
	Audit          *InboxAuditResult     `json:"audit"`
	Reconciliation *InboxReconcileResult `json:"reconciliation,omitempty"`
	Candidates     []FollowupCandidate   `json:"candidates"`
}

type followupCandidateHead struct {
	Latest         EmailMessage
	CampaignName   string
	CampaignStatus string
	LeadStatus     string
	LeadEmail      string
	Company        string
	AccountEmail   string
}

// ReviewFollowupCandidates makes the provider audit a mandatory gate. It never
// returns candidates from stale state. Reconcile is the only mode that writes,
// and it imports provider-confirmed messages without sending email.
func ReviewFollowupCandidates(cfg FollowupCandidatesConfig) (*FollowupCandidatesResult, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	workspaceID := NormalizeWorkspaceID(cfg.WorkspaceID)
	if cfg.Now.IsZero() {
		cfg.Now = time.Now().UTC()
	}
	if cfg.Since.IsZero() {
		cfg.Since = cfg.Now.AddDate(0, 0, -120)
	}
	result := &FollowupCandidatesResult{
		WorkspaceID:  workspaceID,
		Since:        cfg.Since.UTC(),
		MinAgeHours:  int(cfg.MinAge / time.Hour),
		MaxFollowups: cfg.MaxFollowups,
	}
	auditCfg := AuditInboxHistoryConfig{
		DB: cfg.DB, WorkspaceID: workspaceID, Since: cfg.Since,
		SecretResolver: cfg.SecretResolver, GWS: cfg.GWS, IMAP: cfg.IMAP,
	}
	if cfg.Reconcile {
		reconciliation, err := ReconcileInboxHistory(auditCfg)
		result.Reconciliation = reconciliation
		if reconciliation != nil {
			result.Audit = reconciliation.Verification
		}
		if err != nil {
			return result, err
		}
	} else {
		audit, err := AuditInboxHistory(auditCfg)
		result.Audit = audit
		if err != nil {
			return result, fmt.Errorf("provider audit failed; follow-up selection blocked: %w", err)
		}
		if audit.Missing != 0 {
			return result, fmt.Errorf("provider reconciliation required: %d campaign-thread messages are untracked; rerun with --reconcile", audit.Missing)
		}
	}

	candidates, err := FindFollowupCandidates(FindFollowupCandidatesConfig{
		DB: cfg.DB, WorkspaceID: workspaceID, CampaignID: cfg.CampaignID,
		Since: cfg.Since, Now: cfg.Now, MinAge: cfg.MinAge,
		MaxFollowups: cfg.MaxFollowups, Limit: cfg.Limit, IncludeThread: cfg.IncludeThread,
	})
	if err != nil {
		return result, err
	}
	result.Candidates = candidates
	return result, nil
}

func FindFollowupCandidates(cfg FindFollowupCandidatesConfig) ([]FollowupCandidate, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("db is required")
	}
	if cfg.Now.IsZero() {
		cfg.Now = time.Now().UTC()
	}
	if cfg.Since.IsZero() {
		cfg.Since = cfg.Now.AddDate(0, 0, -120)
	}
	if cfg.MinAge < 0 {
		return nil, fmt.Errorf("min_age must not be negative")
	}
	if cfg.MaxFollowups < 0 {
		return nil, fmt.Errorf("max_followups must not be negative")
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	cutoff := cfg.Now.Add(-cfg.MinAge)

	query := `
		SELECT em.campaign_id, em.lead_id, em.account_id, em.message_id, em.thread_id,
			em.from_email, em.to_emails, em.subject, em.text_body, em.display_body,
			em.snippet, em.raw_headers, em.occurred_at,
			c.name, c.status, cl.status, l.email, l.company, a.email
		FROM email_messages em
		JOIN campaigns c ON c.id = em.campaign_id
		JOIN campaign_leads cl ON cl.campaign_id = em.campaign_id AND cl.lead_id = em.lead_id
		JOIN leads l ON l.id = em.lead_id
		JOIN accounts a ON a.id = em.account_id
		WHERE c.workspace_id = ?
		AND em.id = (
			SELECT em2.id FROM email_messages em2
			WHERE em2.campaign_id = em.campaign_id AND em2.lead_id = em.lead_id
			ORDER BY em2.occurred_at DESC, em2.id DESC LIMIT 1
		)
		AND em.direction = 'outbound'
		AND em.type IN ('sent', 'manual_reply')
		AND em.occurred_at >= ? AND em.occurred_at <= ?
		AND em.thread_id <> ''
		AND cl.status = 'replied'
		AND l.global_status = 'active'
		AND a.status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM events suppressed
			WHERE suppressed.campaign_id = em.campaign_id AND suppressed.lead_id = em.lead_id
			AND suppressed.type IN ('unsubscribe', 'bounce')
		)`
	args := []any{NormalizeWorkspaceID(cfg.WorkspaceID), cfg.Since.UTC(), cutoff.UTC()}
	if cfg.CampaignID > 0 {
		query += " AND em.campaign_id = ?"
		args = append(args, cfg.CampaignID)
	}
	query += " ORDER BY em.occurred_at DESC, em.id DESC"

	rows, err := queryDB(cfg.DB, query, args...)
	if err != nil {
		return nil, fmt.Errorf("loading follow-up candidate heads: %w", err)
	}
	defer rows.Close()

	var heads []followupCandidateHead
	for rows.Next() {
		var head followupCandidateHead
		head.Latest.Direction = EmailMessageDirectionOutbound
		head.Latest.Type = EmailMessageTypeManualReply
		if err := rows.Scan(
			&head.Latest.CampaignID, &head.Latest.LeadID, &head.Latest.AccountID,
			&head.Latest.MessageID, &head.Latest.ThreadID, &head.Latest.FromEmail,
			&head.Latest.ToEmails, &head.Latest.Subject, &head.Latest.TextBody,
			&head.Latest.DisplayBody, &head.Latest.Snippet, &head.Latest.RawHeaders,
			&head.Latest.OccurredAt, &head.CampaignName, &head.CampaignStatus,
			&head.LeadStatus, &head.LeadEmail, &head.Company, &head.AccountEmail,
		); err != nil {
			return nil, err
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var candidates []FollowupCandidate
	for _, head := range heads {
		messages, err := ListEmailThreadMessages(cfg.DB, ListEmailThreadMessagesOpts{
			CampaignID: head.Latest.CampaignID,
			LeadID:     head.Latest.LeadID,
			ThreadID:   head.Latest.ThreadID,
			Limit:      500,
		})
		if err != nil {
			return nil, fmt.Errorf("loading campaign %d lead %d thread: %w", head.Latest.CampaignID, head.Latest.LeadID, err)
		}
		candidate, ok, err := followupCandidateFromThread(cfg.DB, cfg.Now, cfg.MaxFollowups, cfg.IncludeThread, head, messages)
		if err != nil {
			return nil, err
		}
		if ok {
			candidates = append(candidates, candidate)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ReplyCount != candidates[j].ReplyCount {
			return candidates[i].ReplyCount > candidates[j].ReplyCount
		}
		return candidates[i].LastOutboundAt.After(candidates[j].LastOutboundAt)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	for index := range candidates {
		candidates[index].Rank = index + 1
	}
	return candidates, nil
}

func followupCandidateFromThread(db *sql.DB, now time.Time, maxFollowups int, includeThread bool, head followupCandidateHead, messages []EmailMessage) (FollowupCandidate, bool, error) {
	if len(messages) == 0 {
		return FollowupCandidate{}, false, nil
	}
	lastInboundIndex := -1
	replyCount := 0
	for index, message := range messages {
		if message.Direction == EmailMessageDirectionInbound && message.Type == EmailMessageTypeReply {
			lastInboundIndex = index
			replyCount++
		}
	}
	if lastInboundIndex < 0 {
		return FollowupCandidate{}, false, nil
	}
	outboundAfterReply := 0
	for _, message := range messages[lastInboundIndex+1:] {
		if message.Direction == EmailMessageDirectionOutbound &&
			(message.Type == EmailMessageTypeSent || message.Type == EmailMessageTypeManualReply) {
			outboundAfterReply++
		}
	}
	if outboundAfterReply < 1 {
		return FollowupCandidate{}, false, nil
	}
	followupCount := outboundAfterReply - 1
	if followupCount > maxFollowups {
		return FollowupCandidate{}, false, nil
	}
	latest := messages[len(messages)-1]
	if latest.Direction != EmailMessageDirectionOutbound ||
		(latest.Type != EmailMessageTypeSent && latest.Type != EmailMessageTypeManualReply) {
		return FollowupCandidate{}, false, nil
	}
	toEmail, err := replyRecipientEmail(db, latest.LeadID, latest)
	if err != nil {
		return FollowupCandidate{}, false, fmt.Errorf("resolving campaign %d lead %d recipient: %w", latest.CampaignID, latest.LeadID, err)
	}
	lastInbound := messages[lastInboundIndex]
	var thread []FollowupThreadMessage
	if includeThread {
		thread = make([]FollowupThreadMessage, 0, len(messages))
		for _, message := range messages {
			thread = append(thread, FollowupThreadMessage{
				Direction: message.Direction, Type: message.Type, FromEmail: message.FromEmail,
				ToEmails: message.ToEmails, Subject: message.Subject,
				Body: followupMessageBody(message), OccurredAt: message.OccurredAt,
			})
		}
	}
	age := now.Sub(latest.OccurredAt)
	if age < 0 {
		age = 0
	}
	return FollowupCandidate{
		CampaignID: latest.CampaignID, CampaignName: head.CampaignName,
		CampaignStatus: head.CampaignStatus, LeadID: latest.LeadID,
		LeadStatus: head.LeadStatus, LeadEmail: head.LeadEmail, Company: head.Company,
		AccountID: latest.AccountID, FromEmail: head.AccountEmail, ToEmail: toEmail,
		Subject: latest.Subject, ThreadID: latest.ThreadID, ReplyCount: replyCount,
		FollowupCount: followupCount, MessageCount: len(messages), AgeDays: int(age / (24 * time.Hour)),
		LastInboundAt: lastInbound.OccurredAt, LastInboundFrom: lastInbound.FromEmail,
		LastInboundBody: followupMessageBody(lastInbound), LastOutboundAt: latest.OccurredAt,
		LastOutboundBody: followupMessageBody(latest), Thread: thread,
	}, true, nil
}

func followupMessageBody(message EmailMessage) string {
	for _, value := range []string{message.DisplayBody, message.TextBody, message.Snippet} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
