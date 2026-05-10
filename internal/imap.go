package internal

import (
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
)

// IMAPMessageLister lists mailbox messages for reply and bounce polling.
type IMAPMessageLister interface {
	ListMessages(account Account, since time.Time, includeSpamTrash bool) ([]GWSMessage, error)
}

// IMAPAccountVerifier verifies IMAP connectivity and authentication.
type IMAPAccountVerifier interface {
	VerifyAccount(account Account) error
}

// IMAPTransport is the production IMAP polling transport.
type IMAPTransport struct {
	Resolver SecretResolver
	Timeout  time.Duration

	Mailboxes      []string
	SpamTrashBoxes []string
	MaxBodyBytes   int64
	openIMAPClient func(account Account, password string) (imapClient, error)
}

type imapClient interface {
	Select(name string, readOnly bool) (*imap.MailboxStatus, error)
	UidSearch(criteria *imap.SearchCriteria) ([]uint32, error)
	UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error
	Logout() error
}

func NewIMAPTransport(resolver SecretResolver) *IMAPTransport {
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	return &IMAPTransport{
		Resolver:       resolver,
		Timeout:        30 * time.Second,
		Mailboxes:      []string{"INBOX"},
		SpamTrashBoxes: []string{"Spam", "Junk", "Junk E-mail", "Trash", "Deleted Items", "[Gmail]/Spam", "[Gmail]/Trash"},
		MaxBodyBytes:   64 * 1024,
	}
}

func (t *IMAPTransport) ListMessages(account Account, since time.Time, includeSpamTrash bool) ([]GWSMessage, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return nil, fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}

	resolver := t.Resolver
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	imapRef := strings.TrimSpace(account.IMAPPasswordRef)
	if imapRef == "" {
		imapRef = account.SMTPPasswordRef
	}
	password, err := resolver.ResolveSecret(imapRef)
	if err != nil {
		return nil, fmt.Errorf("resolving IMAP password for %s: %w", account.Email, err)
	}

	open := t.openIMAPClient
	if open == nil {
		open = func(account Account, password string) (imapClient, error) {
			return t.open(account, password)
		}
	}
	client, err := open(account, password)
	if err != nil {
		return nil, err
	}
	defer client.Logout()

	mailboxes := append([]string{}, t.Mailboxes...)
	if includeSpamTrash {
		mailboxes = append(mailboxes, t.SpamTrashBoxes...)
	}
	if len(mailboxes) == 0 {
		mailboxes = []string{"INBOX"}
	}

	var all []GWSMessage
	for i, mailbox := range mailboxes {
		messages, err := t.listMailboxMessages(client, account, mailbox, since)
		if err != nil {
			if i > 0 {
				continue
			}
			return nil, err
		}
		all = append(all, messages...)
	}
	return dedupeMailboxMessages(all), nil
}

func (t *IMAPTransport) VerifyAccount(account Account) error {
	if account.Provider != AccountProviderSMTPIMAP {
		return fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}

	resolver := t.Resolver
	if resolver == nil {
		resolver = EnvSecretResolver{}
	}
	imapRef := strings.TrimSpace(account.IMAPPasswordRef)
	if imapRef == "" {
		imapRef = account.SMTPPasswordRef
	}
	password, err := resolver.ResolveSecret(imapRef)
	if err != nil {
		return fmt.Errorf("resolving IMAP password for %s: %w", account.Email, err)
	}

	open := t.openIMAPClient
	if open == nil {
		open = func(account Account, password string) (imapClient, error) {
			return t.open(account, password)
		}
	}
	client, err := open(account, password)
	if err != nil {
		return err
	}
	defer client.Logout()

	mailbox := "INBOX"
	if len(t.Mailboxes) > 0 && strings.TrimSpace(t.Mailboxes[0]) != "" {
		mailbox = t.Mailboxes[0]
	}
	if _, err := client.Select(mailbox, true); err != nil {
		return fmt.Errorf("selecting IMAP mailbox %s: %w", mailbox, err)
	}
	return nil
}

func (t *IMAPTransport) open(account Account, password string) (*imapclient.Client, error) {
	if account.IMAPHost == "" {
		return nil, fmt.Errorf("imap host is required")
	}
	if account.IMAPPort < 1 || account.IMAPPort > 65535 {
		return nil, fmt.Errorf("imap port must be between 1 and 65535")
	}
	username := strings.TrimSpace(account.IMAPUsername)
	if username == "" {
		username = account.Email
	}
	if password == "" {
		return nil, fmt.Errorf("imap password is required")
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	addr := net.JoinHostPort(account.IMAPHost, strconv.Itoa(account.IMAPPort))
	dialer := &net.Dialer{Timeout: timeout}

	tlsMode := account.IMAPTLSMode
	if tlsMode == "" {
		tlsMode = "ssl"
	}

	var client *imapclient.Client
	var err error
	if tlsMode == "ssl" {
		client, err = imapclient.DialWithDialerTLS(dialer, addr, &tls.Config{
			ServerName: account.IMAPHost,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		client, err = imapclient.DialWithDialer(dialer, addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to IMAP server: %w", err)
	}
	client.Timeout = timeout

	if tlsMode == "starttls" {
		if ok, _ := client.SupportStartTLS(); !ok {
			_ = client.Logout()
			return nil, fmt.Errorf("IMAP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{
			ServerName: account.IMAPHost,
			MinVersion: tls.VersionTLS12,
		}); err != nil {
			_ = client.Logout()
			return nil, fmt.Errorf("starting IMAP TLS: %w", err)
		}
	}

	if err := client.Login(username, password); err != nil {
		_ = client.Logout()
		return nil, fmt.Errorf("authenticating IMAP user: %w", err)
	}
	return client, nil
}

func (t *IMAPTransport) listMailboxMessages(client imapClient, account Account, mailbox string, since time.Time) ([]GWSMessage, error) {
	if _, err := client.Select(mailbox, true); err != nil {
		return nil, fmt.Errorf("selecting IMAP mailbox %s: %w", mailbox, err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.Since = since.AddDate(0, 0, -1)

	uids, err := client.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("searching IMAP mailbox %s: %w", mailbox, err)
	}
	if len(uids) == 0 {
		return nil, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, section.FetchItem()}

	ch := make(chan *imap.Message, len(uids))
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.UidFetch(seqset, items, ch)
	}()

	var messages []GWSMessage
	for msg := range ch {
		parsed, err := t.parseMessage(account, mailbox, msg, section)
		if err != nil {
			continue
		}
		messages = append(messages, parsed)
	}
	if err := <-errCh; err != nil {
		return nil, fmt.Errorf("fetching IMAP messages from %s: %w", mailbox, err)
	}
	return messages, nil
}

func (t *IMAPTransport) parseMessage(account Account, mailbox string, msg *imap.Message, section *imap.BodySectionName) (GWSMessage, error) {
	body := msg.GetBody(section)
	if body == nil {
		return GWSMessage{}, fmt.Errorf("IMAP message %d has no body", msg.Uid)
	}
	limit := t.MaxBodyBytes
	if limit <= 0 {
		limit = 64 * 1024
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return GWSMessage{}, fmt.Errorf("reading IMAP message %d body: %w", msg.Uid, err)
	}
	return ParseIMAPRawMessage(account.Email, mailbox, msg.Uid, raw, msg.Envelope), nil
}

func ParseIMAPRawMessage(accountEmail, mailbox string, uid uint32, raw []byte, envelope *imap.Envelope) GWSMessage {
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	headers := map[string]string{}
	var snippet string
	var textBody string
	if err == nil {
		for key, values := range parsed.Header {
			headers[textproto.CanonicalMIMEHeaderKey(key)] = strings.Join(values, ", ")
		}
		body, _ := io.ReadAll(io.LimitReader(parsed.Body, 4096))
		textBody = strings.TrimSpace(string(body))
		snippet = textBody
	} else {
		snippet = strings.TrimSpace(string(raw))
		textBody = snippet
	}

	subject := decodeHeader(headers["Subject"])
	from := headers["From"]
	to := headers["To"]
	inReplyTo := normalizeMessageID(headers["In-Reply-To"])
	references := headers["References"]
	messageID := normalizeMessageID(headers["Message-Id"])
	var date time.Time
	if parsed, err := mail.ParseDate(headers["Date"]); err == nil {
		date = parsed.UTC()
	}

	if envelope != nil {
		if subject == "" {
			subject = envelope.Subject
		}
		if date.IsZero() {
			date = envelope.Date.UTC()
		}
		if inReplyTo == "" {
			inReplyTo = normalizeMessageID(envelope.InReplyTo)
		}
		if messageID == "" {
			messageID = normalizeMessageID(envelope.MessageId)
		}
	}
	if messageID == "" {
		messageID = fmt.Sprintf("imap:%s:%s:%d", accountEmail, mailbox, uid)
	}

	threadID := firstMessageID(references)
	if threadID == "" {
		threadID = inReplyTo
	}

	return GWSMessage{
		ID:        messageID,
		ThreadID:  threadID,
		Snippet:   snippet,
		TextBody:  textBody,
		Headers:   headers,
		From:      from,
		To:        to,
		Subject:   subject,
		InReplyTo: inReplyTo,
		Date:      date,
	}
}

func decodeHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func normalizeMessageID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "<") && strings.Contains(value, ">") {
		return value[:strings.Index(value, ">")+1]
	}
	if strings.Contains(value, "@") {
		return "<" + strings.Trim(value, "<>") + ">"
	}
	return value
}

func firstMessageID(value string) string {
	value = strings.TrimSpace(value)
	for {
		start := strings.Index(value, "<")
		if start < 0 {
			return ""
		}
		end := strings.Index(value[start:], ">")
		if end < 0 {
			return ""
		}
		candidate := value[start : start+end+1]
		if strings.Contains(candidate, "@") {
			return candidate
		}
		value = value[start+end+1:]
	}
}

func dedupeMailboxMessages(messages []GWSMessage) []GWSMessage {
	seen := map[string]struct{}{}
	out := make([]GWSMessage, 0, len(messages))
	for _, message := range messages {
		key := message.ID
		if key == "" {
			key = message.From + "\x00" + message.Subject + "\x00" + message.Snippet
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, message)
	}
	return out
}
