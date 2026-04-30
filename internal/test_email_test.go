package internal

import "testing"

func TestNormalizeTestRecipient(t *testing.T) {
	recipient, err := normalizeTestRecipient("Smoke Test <smoke@example.com>")
	if err != nil {
		t.Fatalf("normalizeTestRecipient error: %v", err)
	}
	if recipient != "smoke@example.com" {
		t.Fatalf("expected parsed address, got %q", recipient)
	}
}

func TestNormalizeTestRecipientRejectsEmpty(t *testing.T) {
	_, err := normalizeTestRecipient(" ")
	if err == nil {
		t.Fatal("expected empty recipient error")
	}
}
