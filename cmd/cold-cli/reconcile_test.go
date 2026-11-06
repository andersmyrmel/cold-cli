package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andersmyrmel/cold-cli/internal"
)

func TestReconciliationSummaryDistinguishesPartialZeroCounts(t *testing.T) {
	result := &internal.InboxReconcileResult{WorkspaceID: "workspace-a", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	failed := reconciliationSummary(result, errors.New("provider unavailable"))
	if strings.Contains(failed, "Reconciled workspace") || !strings.HasPrefix(failed, "Reconciliation failed") {
		t.Fatalf("partial zero counts look successful: %q", failed)
	}
	result.Completed = true
	if got := reconciliationSummary(result, nil); !strings.HasPrefix(got, "Reconciled workspace workspace-a") {
		t.Fatalf("successful completion missing: %q", got)
	}
}
