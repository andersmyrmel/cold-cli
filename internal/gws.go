package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// GWSClient is the interface for Gmail operations via gws CLI.
type GWSClient interface {
	SendEmail(account, to, rawMsg string) (msgID, threadID string, err error)
	ListMessages(account, query string) ([]GWSMessage, error)
	GetMessage(account, msgID string) (*GWSMessage, error)
}

// GWSMessage represents a parsed Gmail message from gws output.
type GWSMessage struct {
	ID        string            `json:"id"`
	ThreadID  string            `json:"threadId"`
	Snippet   string            `json:"snippet"`
	LabelIDs  []string          `json:"labelIds"`
	Headers   map[string]string // parsed from payload.headers
	From      string
	To        string
	Subject   string
	InReplyTo string
}

// gws send response
type gwsSendResponse struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

// gws list response
type gwsListResponse struct {
	Messages []struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	} `json:"messages"`
}

// gws get response (full format)
type gwsGetResponse struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Snippet  string `json:"snippet"`
	LabelIDs []string `json:"labelIds"`
	Payload  struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

// GWSCLI is the real implementation that calls gws as a subprocess.
type GWSCLI struct {
	Timeout time.Duration
}

func NewGWSCLI() *GWSCLI {
	return &GWSCLI{Timeout: 30 * time.Second}
}

// SendEmail sends an email via gws and returns the message ID and thread ID.
// rawMsg is a base64url-encoded RFC 2822 message.
func (g *GWSCLI) SendEmail(account, to, rawMsg string) (string, string, error) {
	body := map[string]string{"raw": rawMsg}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshaling send body: %w", err)
	}

	// gws uses userId "me" for the currently authenticated account
	out, stderr, err := g.run("gmail", "users", "messages", "send",
		"--params", `{"userId": "me"}`,
		"--json", string(bodyJSON))
	if err != nil {
		return "", "", fmt.Errorf("gws send failed: %w\nstderr: %s", err, stderr)
	}

	var resp gwsSendResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", "", fmt.Errorf("parsing gws send response: %w\nraw output: %s", err, out)
	}

	if resp.ID == "" {
		return "", "", fmt.Errorf("gws returned empty message ID")
	}
	if resp.ThreadID == "" {
		return "", "", fmt.Errorf("gws returned empty thread ID")
	}

	return resp.ID, resp.ThreadID, nil
}

// ListMessages lists messages matching a Gmail search query.
func (g *GWSCLI) ListMessages(account, query string) ([]GWSMessage, error) {
	params := map[string]any{
		"userId":     "me",
		"q":          query,
		"maxResults": 100,
	}
	paramsJSON, _ := json.Marshal(params)

	out, stderr, err := g.run("gmail", "users", "messages", "list",
		"--params", string(paramsJSON))
	if err != nil {
		return nil, fmt.Errorf("gws list failed: %w\nstderr: %s", err, stderr)
	}

	// Handle empty results (gws may return empty or null)
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}

	var resp gwsListResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing gws list response: %w\nraw: %s", err, out)
	}

	// For each message, we only get IDs from list. Need to GET each for headers.
	var messages []GWSMessage
	for _, m := range resp.Messages {
		msg, err := g.GetMessage(account, m.ID)
		if err != nil {
			return nil, fmt.Errorf("getting message %s: %w", m.ID, err)
		}
		messages = append(messages, *msg)
	}

	return messages, nil
}

// GetMessage retrieves a single message with full payload (headers).
func (g *GWSCLI) GetMessage(account, msgID string) (*GWSMessage, error) {
	params := map[string]any{
		"userId": "me",
		"id":     msgID,
		"format": "full",
	}
	paramsJSON, _ := json.Marshal(params)

	out, stderr, err := g.run("gmail", "users", "messages", "get",
		"--params", string(paramsJSON))
	if err != nil {
		return nil, fmt.Errorf("gws get failed: %w\nstderr: %s", err, stderr)
	}

	var resp gwsGetResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing gws get response: %w\nraw: %s", err, out)
	}

	msg := &GWSMessage{
		ID:       resp.ID,
		ThreadID: resp.ThreadID,
		Snippet:  resp.Snippet,
		LabelIDs: resp.LabelIDs,
		Headers:  make(map[string]string),
	}

	for _, h := range resp.Payload.Headers {
		msg.Headers[h.Name] = h.Value
		switch h.Name {
		case "From":
			msg.From = h.Value
		case "To":
			msg.To = h.Value
		case "Subject":
			msg.Subject = h.Value
		case "In-Reply-To":
			msg.InReplyTo = h.Value
		}
	}

	return msg, nil
}

// gwsErrorResponse checks if gws returned a JSON error (API error with 200 exit code).
type gwsErrorResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (g *GWSCLI) run(args ...string) (stdout, stderr []byte, err error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gws", args...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return outBuf.Bytes(), errBuf.Bytes(), err
	}

	// Check for API error in JSON response (gws returns exit 0 even on API errors)
	out := outBuf.Bytes()
	var errResp gwsErrorResponse
	if json.Unmarshal(out, &errResp) == nil && errResp.Error != nil {
		return out, errBuf.Bytes(), fmt.Errorf("gws API error %d: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return out, errBuf.Bytes(), nil
}

// CheckGWSInstalled verifies gws binary is available on PATH.
func CheckGWSInstalled() error {
	_, err := exec.LookPath("gws")
	if err != nil {
		return fmt.Errorf("gws CLI not found on PATH — install from https://github.com/nicholasgasior/gws")
	}
	return nil
}

// MockGWS is a test implementation that records calls and returns canned responses.
type MockGWS struct {
	SentEmails   []MockSentEmail
	InboxMessages []GWSMessage

	// Control behavior
	SendError    error
	SendMsgID    string
	SendThreadID string
	ListError    error
	GetError     error
}

type MockSentEmail struct {
	Account string
	To      string
	RawMsg  string
}

func (m *MockGWS) SendEmail(account, to, rawMsg string) (string, string, error) {
	if m.SendError != nil {
		return "", "", m.SendError
	}
	m.SentEmails = append(m.SentEmails, MockSentEmail{
		Account: account,
		To:      to,
		RawMsg:  rawMsg,
	})

	msgID := m.SendMsgID
	if msgID == "" {
		msgID = fmt.Sprintf("msg-%d", len(m.SentEmails))
	}
	threadID := m.SendThreadID
	if threadID == "" {
		threadID = fmt.Sprintf("thread-%d", len(m.SentEmails))
	}

	return msgID, threadID, nil
}

func (m *MockGWS) ListMessages(account, query string) ([]GWSMessage, error) {
	if m.ListError != nil {
		return nil, m.ListError
	}
	return m.InboxMessages, nil
}

func (m *MockGWS) GetMessage(account, msgID string) (*GWSMessage, error) {
	if m.GetError != nil {
		return nil, m.GetError
	}
	for _, msg := range m.InboxMessages {
		if msg.ID == msgID {
			return &msg, nil
		}
	}
	return nil, fmt.Errorf("message %s not found", msgID)
}
