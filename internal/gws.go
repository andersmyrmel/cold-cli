package internal

import (
	"fmt"
	"os/exec"
)

// GWSClient is the interface for Gmail operations via gws CLI.
type GWSClient interface {
	SendEmail(account, to, rawMsg string) (msgID, threadID string, err error)
	ListMessages(account, query string) ([]Message, error)
}

// Message represents a parsed Gmail message from gws output.
type Message struct {
	ID        string
	ThreadID  string
	From      string
	Subject   string
	InReplyTo string
	Body      string
}

// GWSCLI is the real implementation that calls gws as a subprocess.
type GWSCLI struct{}

func (g *GWSCLI) SendEmail(account, to, rawMsg string) (string, string, error) {
	// Implemented in Phase 4
	return "", "", fmt.Errorf("not yet implemented")
}

func (g *GWSCLI) ListMessages(account, query string) ([]Message, error) {
	// Implemented in Phase 4
	return nil, fmt.Errorf("not yet implemented")
}

// CheckGWSInstalled verifies gws binary is available on PATH.
func CheckGWSInstalled() error {
	path, err := exec.LookPath("gws")
	if err != nil {
		return fmt.Errorf("gws CLI not found on PATH — install from https://github.com/nicholasgasior/gws")
	}
	_ = path
	return nil
}
