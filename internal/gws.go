package internal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GWSClient is the interface for Gmail operations via gws CLI.
type GWSClient interface {
	SendEmail(account, to, rawMsg, threadID string) (msgID, sentThreadID string, err error)
	ListMessages(account, query string, includeSpamTrash ...bool) ([]GWSMessage, error)
	GetMessage(account, msgID string) (*GWSMessage, error)
}

// GWSMessage represents a parsed Gmail message from gws output.
type GWSMessage struct {
	ID        string `json:"id"`
	ThreadID  string `json:"threadId"`
	Snippet   string `json:"snippet"`
	TextBody  string
	HTMLBody  string
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
	ID       string         `json:"id"`
	ThreadID string         `json:"threadId"`
	Snippet  string         `json:"snippet"`
	LabelIDs []string       `json:"labelIds"`
	Payload  gwsMessagePart `json:"payload"`
}

type gwsMessagePart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Parts []gwsMessagePart `json:"parts"`
}

// GWSCLI is the real implementation that calls gws as a subprocess.
type GWSCLI struct {
	Timeout    time.Duration
	ConfigDirs map[string]string // account email → gws config dir
}

func NewGWSCLI() *GWSCLI {
	return &GWSCLI{
		Timeout:    30 * time.Second,
		ConfigDirs: map[string]string{},
	}
}

// SetConfigDir registers a gws config directory for a specific account.
func (g *GWSCLI) SetConfigDir(account, configDir string) {
	g.ConfigDirs[account] = configDir
}

// SendEmail sends an email via gws and returns the Gmail message ID and thread ID.
func (g *GWSCLI) SendEmail(account, to, rawMsg, threadID string) (string, string, error) {
	body := map[string]any{"raw": rawMsg}
	if threadID != "" {
		body["threadId"] = threadID
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshaling send body: %w", err)
	}

	out, stderr, err := g.run(account, "gmail", "users", "messages", "send",
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
func (g *GWSCLI) ListMessages(account, query string, includeSpamTrash ...bool) ([]GWSMessage, error) {
	params := map[string]any{
		"userId":     "me",
		"q":          query,
		"maxResults": 25,
	}
	if len(includeSpamTrash) > 0 && includeSpamTrash[0] {
		params["includeSpamTrash"] = true
	}
	paramsJSON, _ := json.Marshal(params)

	out, stderr, err := g.run(account, "gmail", "users", "messages", "list",
		"--params", string(paramsJSON))
	if err != nil {
		return nil, fmt.Errorf("gws list failed: %w\nstderr: %s", err, stderr)
	}

	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}

	var resp gwsListResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parsing gws list response: %w\nraw: %s", err, out)
	}

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

	out, stderr, err := g.run(account, "gmail", "users", "messages", "get",
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
	msg.TextBody, msg.HTMLBody = extractGWSMessageBodies(resp.Payload)

	return msg, nil
}

func extractGWSMessageBodies(part gwsMessagePart) (textBody string, htmlBody string) {
	if part.Body.Data != "" {
		body, err := decodeGWSBody(part.Body.Data)
		if err == nil {
			switch strings.ToLower(part.MimeType) {
			case "text/plain":
				textBody = body
			case "text/html":
				htmlBody = body
			}
		}
	}

	for _, child := range part.Parts {
		childText, childHTML := extractGWSMessageBodies(child)
		if textBody == "" && childText != "" {
			textBody = childText
		}
		if htmlBody == "" && childHTML != "" {
			htmlBody = childHTML
		}
	}

	return textBody, htmlBody
}

func decodeGWSBody(data string) (string, error) {
	decoded, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(data)
	}
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// gwsErrorResponse checks if gws returned a JSON error (API error with 200 exit code).
type gwsErrorResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (g *GWSCLI) run(account string, args ...string) (stdout, stderr []byte, err error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gws", args...)

	// Set per-account gws config dir if registered
	if configDir, ok := g.ConfigDirs[account]; ok && configDir != "" {
		cmd.Env = append(os.Environ(), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+configDir)
	}

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
		return fmt.Errorf("gws CLI not found on PATH — install from https://github.com/googleworkspace/cli")
	}
	return nil
}

// GWSConfigDirForAccount returns the config dir path for an account.
// Creates the directory and copies client_secret.json from the default gws config.
func GWSConfigDirForAccount(email string) string {
	safe := strings.ReplaceAll(email, "@", "-at-")
	safe = strings.ReplaceAll(safe, ".", "-")
	dir := filepath.Join(DataDir(), "gws-accounts", safe)
	os.MkdirAll(dir, 0700)

	// Copy client_secret.json from default gws config if not present
	destSecret := filepath.Join(dir, "client_secret.json")
	if _, err := os.Stat(destSecret); os.IsNotExist(err) {
		home, _ := os.UserHomeDir()
		srcSecret := filepath.Join(home, ".config", "gws", "client_secret.json")
		if data, err := os.ReadFile(srcSecret); err == nil {
			os.WriteFile(destSecret, data, 0600)
		}
	}

	return dir
}

// GWSAuthLogin runs 'gws auth login' for a specific account config dir.
func GWSAuthLogin(configDir string) error {
	cmd := exec.Command("gws", "auth", "login")
	cmd.Env = append(os.Environ(), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+configDir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
