package internal

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	imap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"golang.org/x/net/html/charset"
)

// IMAPMessageLister lists mailbox messages for reply and bounce polling.
type IMAPMessageLister interface {
	ListMessages(account Account, since time.Time, includeSpamTrash bool) ([]GWSMessage, error)
}

type IMAPThreadMessageLister interface {
	ListThreadMessages(account Account, since time.Time, messageIDs []string) ([]GWSMessage, error)
}

// IMAPSentAppender stores an SMTP-delivered message in the account's Sent
// mailbox so webmail and other IMAP clients show the same conversation.
type IMAPSentAppender interface {
	AppendSent(account Account, params EmailParams) (mailbox string, err error)
}

// IMAPAccountVerifier verifies IMAP connectivity and authentication.
type IMAPAccountVerifier interface {
	VerifyAccount(account Account) error
}

// IMAPTransport is the production IMAP polling transport.
type IMAPTransport struct {
	Resolver SecretResolver
	Timeout  time.Duration

	Mailboxes              []string
	SpamTrashBoxes         []string
	SentMailboxes          []string
	MaxBodyBytes           int64
	SearchRateLimitRetries int
	openIMAPClient         func(account Account, password string) (imapClient, error)
	appendMessage          func(account Account, password, mailbox string, flags []string, date time.Time, msg []byte) error
}

type imapClient interface {
	List(ref, name string, ch chan *imap.MailboxInfo) error
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
		SentMailboxes:  []string{"Sent"},
		MaxBodyBytes:   64 * 1024,
	}
}

func (t *IMAPTransport) AppendSent(account Account, params EmailParams) (string, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return "", fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	if err := ValidateEmailParamsHeaders(params); err != nil {
		return "", err
	}
	if strings.TrimSpace(params.MessageID) == "" {
		return "", fmt.Errorf("message-id is required before appending a Sent copy")
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
		return "", fmt.Errorf("resolving IMAP password for %s: %w", account.Email, err)
	}

	mailboxes := t.SentMailboxes
	if len(mailboxes) == 0 {
		mailboxes = []string{"Sent"}
	}
	raw := []byte(BuildRFCMessage(params))
	appendMessage := t.appendMessage
	if appendMessage == nil {
		appendMessage = t.appendToMailbox
	}

	var lastErr error
	for _, mailbox := range mailboxes {
		mailbox = strings.TrimSpace(mailbox)
		if mailbox == "" {
			continue
		}
		if err := appendMessage(account, password, mailbox, []string{imap.SeenFlag}, params.Date, raw); err == nil {
			return mailbox, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Sent mailbox configured")
	}
	return "", fmt.Errorf("appending message to Sent mailbox: %w", lastErr)
}

func (t *IMAPTransport) appendToMailbox(account Account, password, mailbox string, flags []string, date time.Time, msg []byte) error {
	client, err := t.open(account, password)
	if err != nil {
		return err
	}
	defer client.Logout()
	if err := client.Append(mailbox, flags, date, bytes.NewBuffer(msg)); err != nil {
		return fmt.Errorf("appending to IMAP mailbox %s: %w", mailbox, err)
	}
	return nil
}

func (t *IMAPTransport) ListMessages(account Account, since time.Time, includeSpamTrash bool) ([]GWSMessage, error) {
	mailboxes := append([]string{}, t.Mailboxes...)
	if includeSpamTrash {
		mailboxes = append(mailboxes, t.SpamTrashBoxes...)
	}
	if len(mailboxes) == 0 {
		mailboxes = []string{"INBOX"}
	}
	return t.listMessagesFromMailboxes(account, since, uniqueMailboxNames(mailboxes), nil)
}

// ListThreadMessages searches Inbox and Sent server-side by RFC threading
// headers so refreshing one conversation does not download every message in
// the account since the campaign began.
func (t *IMAPTransport) ListThreadMessages(account Account, since time.Time, messageIDs []string) ([]GWSMessage, error) {
	return t.listDiscoveredMailboxMessages(account, since, messageIDs)
}

func (t *IMAPTransport) listMessagesFromMailboxes(account Account, since time.Time, mailboxes []string, messageIDs []string) ([]GWSMessage, error) {
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

	return t.listMessagesWithClient(client, account, since, mailboxes, messageIDs, false)
}

func (t *IMAPTransport) listDiscoveredMailboxMessages(account Account, since time.Time, messageIDs []string) ([]GWSMessage, error) {
	if account.Provider != AccountProviderSMTPIMAP {
		return nil, fmt.Errorf("account %s is provider %s, expected %s", account.Email, account.Provider, AccountProviderSMTPIMAP)
	}
	password, err := t.resolvePassword(account)
	if err != nil {
		return nil, err
	}
	open := t.openIMAPClient
	if open == nil {
		open = func(account Account, password string) (imapClient, error) { return t.open(account, password) }
	}
	client, err := open(account, password)
	if err != nil {
		return nil, err
	}
	defer client.Logout()
	mailboxes, err := listSelectableIMAPMailboxes(client)
	if err != nil {
		return nil, fmt.Errorf("listing IMAP mailboxes: %w", err)
	}
	return t.listMessagesWithClient(client, account, since, mailboxes, messageIDs, true)
}

func (t *IMAPTransport) resolvePassword(account Account) (string, error) {
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
		return "", fmt.Errorf("resolving IMAP password for %s: %w", account.Email, err)
	}
	return password, nil
}

func (t *IMAPTransport) listMessagesWithClient(client imapClient, account Account, since time.Time, mailboxes []string, messageIDs []string, strict bool) ([]GWSMessage, error) {
	var all []GWSMessage
	for i, mailbox := range mailboxes {
		messages, err := t.listMailboxMessages(client, account, mailbox, since, messageIDs)
		if err != nil {
			if !strict && i > 0 {
				continue
			}
			return nil, err
		}
		all = append(all, messages...)
	}
	return dedupeMailboxMessages(all), nil
}

func listSelectableIMAPMailboxes(client imapClient) ([]string, error) {
	mailboxCh := make(chan *imap.MailboxInfo, 32)
	errCh := make(chan error, 1)
	go func() { errCh <- client.List("", "*", mailboxCh) }()
	var mailboxes []string
	for mailbox := range mailboxCh {
		if mailbox == nil || strings.TrimSpace(mailbox.Name) == "" || hasIMAPMailboxAttribute(mailbox.Attributes, "\\Noselect") || hasIMAPMailboxAttribute(mailbox.Attributes, "\\Drafts") {
			continue
		}
		mailboxes = append(mailboxes, mailbox.Name)
	}
	if err := <-errCh; err != nil {
		return nil, err
	}
	mailboxes = uniqueMailboxNames(mailboxes)
	if len(mailboxes) == 0 {
		return nil, fmt.Errorf("server returned no selectable mailboxes")
	}
	return mailboxes, nil
}

func hasIMAPMailboxAttribute(attributes []string, wanted string) bool {
	for _, attribute := range attributes {
		if strings.EqualFold(strings.TrimSpace(attribute), wanted) {
			return true
		}
	}
	return false
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

func (t *IMAPTransport) listMailboxMessages(client imapClient, account Account, mailbox string, since time.Time, messageIDs []string) ([]GWSMessage, error) {
	if _, err := client.Select(mailbox, true); err != nil {
		return nil, fmt.Errorf("selecting IMAP mailbox %s: %w", mailbox, err)
	}

	criteria := imapThreadSearchCriteria(since, messageIDs)

	uids, err := client.UidSearch(criteria)
	for attempt := 0; err != nil && attempt < t.SearchRateLimitRetries; attempt++ {
		wait, ok := imapSearchRateLimitWait(err)
		if !ok {
			break
		}
		time.Sleep(wait)
		uids, err = client.UidSearch(criteria)
	}
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

func imapThreadSearchCriteria(since time.Time, messageIDs []string) *imap.SearchCriteria {
	criteria := imap.NewSearchCriteria()
	criteria.Since = since.AddDate(0, 0, -1)
	var terms []*imap.SearchCriteria
	seen := map[string]struct{}{}
	for _, messageID := range messageIDs {
		messageID = normalizeMessageID(messageID)
		if !looksLikeMessageID(messageID) {
			continue
		}
		key := strings.ToLower(messageID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		for _, headerName := range []string{"Message-ID", "References", "In-Reply-To"} {
			term := imap.NewSearchCriteria()
			term.Header.Set(headerName, messageID)
			terms = append(terms, term)
		}
	}
	if len(terms) == 1 {
		criteria.Header = terms[0].Header
	} else if len(terms) > 1 {
		criteria.Or = append(criteria.Or, [2]*imap.SearchCriteria{terms[0], foldIMAPOrCriteria(terms[1:])})
	}
	return criteria
}

var imapSearchRateLimitPattern = regexp.MustCompile(`(?i)please wait ([0-9]+)s`)

func imapSearchRateLimitWait(err error) (time.Duration, bool) {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "search rate limit exceeded") {
		return 0, false
	}
	wait := 60 * time.Second
	if match := imapSearchRateLimitPattern.FindStringSubmatch(err.Error()); len(match) == 2 {
		if seconds, parseErr := strconv.Atoi(match[1]); parseErr == nil {
			wait = time.Duration(seconds+1) * time.Second
		}
	}
	return wait, true
}

func foldIMAPOrCriteria(terms []*imap.SearchCriteria) *imap.SearchCriteria {
	if len(terms) == 1 {
		return terms[0]
	}
	criteria := imap.NewSearchCriteria()
	criteria.Or = append(criteria.Or, [2]*imap.SearchCriteria{terms[0], foldIMAPOrCriteria(terms[1:])})
	return criteria
}

func uniqueMailboxNames(mailboxes []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		mailbox = strings.TrimSpace(mailbox)
		if mailbox == "" {
			continue
		}
		key := strings.ToLower(mailbox)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, mailbox)
	}
	return result
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
	var htmlBody string
	var mimeType string
	if err == nil {
		for key, values := range parsed.Header {
			headers[textproto.CanonicalMIMEHeaderKey(key)] = strings.Join(values, ", ")
		}
		mimeType, _, _ = mime.ParseMediaType(parsed.Header.Get("Content-Type"))
		textBody, htmlBody = extractIMAPMessageBodies(textproto.MIMEHeader(parsed.Header), parsed.Body)
		snippet = textBody
		if snippet == "" && htmlBody != "" {
			snippet = emailHTMLToText(htmlBody)
		}
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
		HTMLBody:  htmlBody,
		MimeType:  mimeType,
		Headers:   headers,
		From:      from,
		To:        to,
		Subject:   subject,
		InReplyTo: inReplyTo,
		Date:      date,
	}
}

func extractIMAPMessageBodies(header textproto.MIMEHeader, body io.Reader) (string, string) {
	contentType := strings.TrimSpace(header.Get("Content-Type"))
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	mediaType = strings.ToLower(mediaType)

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", ""
		}
		reader := multipart.NewReader(body, boundary)
		var textParts, htmlParts []string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			partText, partHTML := extractIMAPMessageBodies(part.Header, part)
			if strings.TrimSpace(partText) != "" {
				textParts = append(textParts, strings.TrimSpace(partText))
			}
			if strings.TrimSpace(partHTML) != "" {
				htmlParts = append(htmlParts, strings.TrimSpace(partHTML))
			}
		}
		return strings.Join(textParts, "\n\n"), strings.Join(htmlParts, "\n")
	}

	if disposition, _, _ := mime.ParseMediaType(header.Get("Content-Disposition")); strings.EqualFold(disposition, "attachment") {
		return "", ""
	}
	decoded := decodeIMAPTransferEncoding(body, header.Get("Content-Transfer-Encoding"))
	if contentType != "" {
		if charsetDecoded, err := charset.NewReader(decoded, contentType); err == nil {
			decoded = charsetDecoded
		}
	}
	content, err := io.ReadAll(decoded)
	if err != nil {
		return "", ""
	}
	value := strings.TrimSpace(strings.ToValidUTF8(string(content), "�"))
	switch mediaType {
	case "text/plain":
		return value, ""
	case "text/html":
		return "", value
	default:
		return "", ""
	}
}

func decodeIMAPTransferEncoding(body io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		return quotedprintable.NewReader(body)
	default:
		return body
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
