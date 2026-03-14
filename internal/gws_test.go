package internal

import (
	"os/exec"
	"testing"
)

func TestCheckGWSInstalled(t *testing.T) {
	err := CheckGWSInstalled()

	// If gws is on PATH, this should succeed; if not, it should return a clear error
	_, lookErr := exec.LookPath("gws")
	if lookErr != nil {
		if err == nil {
			t.Error("expected error when gws not on PATH")
		}
	} else {
		if err != nil {
			t.Errorf("unexpected error when gws is on PATH: %v", err)
		}
	}
}
