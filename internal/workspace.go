package internal

import (
	"os"
	"strings"
)

const (
	DefaultWorkspaceID = "default"
	WorkspaceEnvVar    = "COLD_CLI_WORKSPACE_ID"
)

func NormalizeWorkspaceID(workspaceID string) string {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return DefaultWorkspaceID
	}
	return strings.ToLower(workspaceID)
}

func WorkspaceIDFromEnv() string {
	return NormalizeWorkspaceID(os.Getenv(WorkspaceEnvVar))
}
