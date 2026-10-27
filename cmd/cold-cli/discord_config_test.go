package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andersmyrmel/cold-cli/internal"
)

func TestConfiguredDiscordNotifierFromEnv(t *testing.T) {
	t.Setenv("COLD_CLI_DISCORD_NOTIFY", "1")
	t.Setenv("DISCORD_WEBHOOK_URL", "https://example.com/shared")
	t.Setenv("DISCORD_WEBHOOK_USERNAME", "Example replies")
	t.Setenv("DISCORD_WEBHOOK_AVATAR_URL", "https://example.com/avatar.png")
	t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", `{"workspace-a":"https://example.com/a","workspace-b":"https://example.com/b"}`)

	notifier, err := configuredDiscordNotifierFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	webhook, ok := notifier.(internal.DiscordWebhookNotifier)
	if !ok || len(webhook.WorkspaceWebhookURLs) != 2 || webhook.WorkspaceWebhookURLs["workspace-a"] != "https://example.com/a" {
		t.Fatalf("unexpected routed notifier: %#v", notifier)
	}
	if webhook.Username != "Example replies" || webhook.AvatarURL != "https://example.com/avatar.png" {
		t.Fatalf("webhook branding was not preserved: %#v", webhook)
	}

	t.Setenv("DISCORD_WEBHOOK_URL", "")
	if notifier, err = configuredDiscordNotifierFromEnv(); err != nil || notifier == nil {
		t.Fatalf("routes should work without a shared webhook: notifier=%#v err=%v", notifier, err)
	}
	t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", "{}")
	notifier, err = configuredDiscordNotifierFromEnv()
	if err != nil || notifier.(internal.DiscordWebhookNotifier).WorkspaceWebhookURLs == nil {
		t.Fatalf("empty routing map must remain explicit: notifier=%#v err=%v", notifier, err)
	}
}

func TestConfiguredDiscordNotifierRejectsInvalidRoutesWithoutFallback(t *testing.T) {
	t.Setenv("COLD_CLI_DISCORD_NOTIFY", "1")
	t.Setenv("DISCORD_WEBHOOK_URL", "https://example.com/shared")
	for _, raw := range []string{
		"", " ", "null", "[]", `{"workspace-a":`,
		`{"workspace-a":1}`, `{"workspace-a":null}`, `{"workspace-a":""}`,
		`{"workspace-a":"https://"}`, `{"workspace-a":"/relative"}`,
		`{"workspace-a":"ftp://example.com/private-token"}`,
		`{"":"https://example.com/private-token"}`,
		`{"Workspace-A":"https://example.com/private-token"}`,
		`{" workspace-a ":"https://example.com/private-token"}`,
		`{"*":"https://example.com/private-token"}`,
		`{"all":"https://example.com/private-token"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", raw)
			notifier, err := configuredDiscordNotifierFromEnv()
			if err == nil || notifier != nil {
				t.Fatalf("expected configuration error without shared fallback, got notifier=%#v err=%v", notifier, err)
			}
			if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("configuration error exposed a webhook URL: %v", err)
			}
		})
	}
	// Disabling notifications also disables routing validation.
	t.Setenv("COLD_CLI_DISCORD_NOTIFY", "0")
	t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", "invalid")
	if notifier, err := configuredDiscordNotifierFromEnv(); err != nil || notifier != nil {
		t.Fatalf("disabled notifications should not validate routes: notifier=%#v err=%v", notifier, err)
	}
}

func TestConfiguredDiscordNotifierSharedWebhook(t *testing.T) {
	t.Setenv("COLD_CLI_DISCORD_NOTIFY", "1")
	// LookupEnv distinguishes an absent routing map from an invalid empty value.
	t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", "restore-after-test")
	if err := os.Unsetenv("COLD_CLI_DISCORD_WEBHOOKS"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCORD_WEBHOOK_URL", "https://example.com/shared")
	notifier, err := configuredDiscordNotifierFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	webhook, ok := notifier.(internal.DiscordWebhookNotifier)
	if !ok || webhook.WorkspaceWebhookURLs != nil || webhook.WebhookURL != "https://example.com/shared" {
		t.Fatalf("legacy shared webhook was not preserved: %#v", notifier)
	}
	t.Setenv("DISCORD_WEBHOOK_URL", "")
	if notifier, err = configuredDiscordNotifierFromEnv(); err != nil || notifier != nil {
		t.Fatalf("expected notifications disabled without any webhooks: notifier=%#v err=%v", notifier, err)
	}
}

func TestInboxReconciliationFailureUsesWorkspaceRoute(t *testing.T) {
	requests := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("COLD_CLI_DISCORD_NOTIFY", "1")
	t.Setenv("DISCORD_WEBHOOK_URL", server.URL+"/shared")
	t.Setenv("COLD_CLI_DISCORD_WEBHOOKS", fmt.Sprintf(`{"workspace-a":%q}`, server.URL+"/a"))
	previousWorkspace := workspaceFlag
	workspaceFlag = "workspace-a"
	t.Cleanup(func() { workspaceFlag = previousWorkspace })
	notifyInboxReconciliationFailure(fmt.Errorf("example provider audit failed"))
	if len(requests) != 1 || <-requests != "/a" {
		t.Fatal("reconciliation failure did not reach the workspace route")
	}
	workspaceFlag = "workspace-b"
	notifyInboxReconciliationFailure(fmt.Errorf("example provider audit failed"))
	if len(requests) != 0 {
		t.Fatal("unmapped reconciliation failure reached a webhook")
	}
}
