package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os/exec"
	"strings"
	"time"
)

// PreSendCheckConfig runs a trusted executable without a shell. Stdin contains
// one recipient email or known phone number per line, matching contact-check
// CLIs. Exit 0 permits delivery, 2 means suppressed, all other results fail closed.
type PreSendCheckConfig struct {
	Command        []string `yaml:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

var ErrContactSuppressed = errors.New("contact is on the do-not-contact list")

// checkBeforeSend deliberately reloads configuration and checks live contacts
// for every delivery. This is shared by scheduled steps and manual inbox replies.
func checkBeforeSend(account Account, params EmailParams, fields map[string]string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading pre-send checks: %w", err)
	}
	check, configured := cfg.PreSendChecks[NormalizeWorkspaceID(account.WorkspaceID)]
	if !configured {
		return nil
	}
	contacts, err := sendCheckContacts(params, fields)
	if err != nil {
		return err
	}
	return runPreSendCheck(check, contacts)
}

func sendCheckContacts(params EmailParams, fields map[string]string) ([]string, error) {
	var contacts []string
	seen := map[string]bool{}
	appendContact := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			contacts = append(contacts, value)
		}
	}
	for _, raw := range append([]string{params.ToEmail}, params.CcEmails...) {
		address, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient for pre-send check")
		}
		appendContact(strings.ToLower(address.Address))
	}
	for _, key := range []string{"phone", "phones", "phone_number", "phone_numbers", "mobile", "mobile_phone", "telephone", "telephone_number"} {
		raw := strings.TrimSpace(fields[key])
		if raw == "" {
			continue
		}
		var phones []string
		if strings.HasPrefix(raw, "[") {
			if err := json.Unmarshal([]byte(raw), &phones); err != nil {
				return nil, fmt.Errorf("invalid phone list for pre-send check")
			}
		} else {
			phones = strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '|' || r == '\n' || r == '\r' })
		}
		for _, phone := range phones {
			appendContact(phone)
		}
	}
	if len(contacts) == 0 {
		return nil, fmt.Errorf("no contacts for pre-send check")
	}
	return contacts, nil
}

func runPreSendCheck(check PreSendCheckConfig, contacts []string) error {
	if len(check.Command) == 0 || strings.TrimSpace(check.Command[0]) == "" || check.TimeoutSeconds < 0 {
		return fmt.Errorf("invalid pre-send check configuration")
	}
	timeout := time.Duration(check.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, check.Command[0], check.Command[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(strings.Join(contacts, "\n") + "\n")
	// Checker output may contain private opt-out notes or connection secrets.
	// Only the exit status is needed to make the delivery decision.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("pre-send check timed out; delivery blocked")
	}
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 2 {
			return ErrContactSuppressed
		}
		return fmt.Errorf("pre-send check failed (exit %d); delivery blocked", exitErr.ExitCode())
	}
	return fmt.Errorf("pre-send check could not start; delivery blocked")
}
