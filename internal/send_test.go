package internal

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestBuildRawMessage_Step1(t *testing.T) {
	raw := BuildRawMessage(EmailParams{
		FromName:  "Anders",
		FromEmail: "anders@example.com",
		ToEmail:   "john@acme.com",
		Subject:   "Quick question about Acme",
		Body:      "Hi John, wanted to chat about Acme.",
	})

	// Decode and check headers
	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decoding base64: %v", err)
	}
	msg := string(decoded)

	checks := []struct {
		name     string
		contains string
	}{
		{"From header", "From: Anders <anders@example.com>"},
		{"To header", "To: john@acme.com"},
		{"Subject header", "Subject: Quick question about Acme"},
		{"Content-Type", "Content-Type: text/html; charset=utf-8"},
		{"Body", "<div>Hi John, wanted to chat about Acme.</div>"},
	}

	for _, c := range checks {
		if !strings.Contains(msg, c.contains) {
			t.Errorf("%s: expected message to contain %q\nmessage:\n%s", c.name, c.contains, msg)
		}
	}

	// Should NOT have threading headers
	if strings.Contains(msg, "In-Reply-To") {
		t.Error("step 1 message should not have In-Reply-To header")
	}
	if strings.Contains(msg, "References") {
		t.Error("step 1 message should not have References header")
	}
}

func TestBuildRawMessage_FollowUp(t *testing.T) {
	raw := BuildRawMessage(EmailParams{
		FromName:  "Anders",
		FromEmail: "anders@example.com",
		ToEmail:   "john@acme.com",
		Subject:   "Re: Quick question about Acme",
		Body:      "Following up...",
		InReplyTo: "<abc123@gmail.com>",
		References: "<abc123@gmail.com>",
	})

	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)

	if !strings.Contains(msg, "In-Reply-To: <abc123@gmail.com>") {
		t.Error("missing In-Reply-To header")
	}
	if !strings.Contains(msg, "References: <abc123@gmail.com>") {
		t.Error("missing References header")
	}
	if !strings.Contains(msg, "Subject: Re: Quick question about Acme") {
		t.Error("missing Re: subject")
	}
}

func TestBuildRawMessage_NoFromName(t *testing.T) {
	raw := BuildRawMessage(EmailParams{
		FromEmail: "anders@example.com",
		ToEmail:   "john@acme.com",
		Subject:   "Hi",
		Body:      "Hello",
	})

	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)

	if !strings.Contains(msg, "From: anders@example.com\r\n") {
		t.Errorf("expected plain From header, got:\n%s", msg)
	}
}

func TestBuildEmailForSend_BaseVariant(t *testing.T) {
	seq := &Sequence{
		Defaults: SequenceDefaults{FromName: "Anders"},
		Steps: []SequenceStep{
			{
				Step:    1,
				Subject: "Hi {{first_name}}",
				Body:    "Hello {{first_name}} at {{company}}",
				Variants: []SequenceVariant{
					{Subject: "{{company}} intro", Body: "Alt body for {{first_name}}"},
				},
			},
		},
	}

	lead := map[string]string{
		"email":      "john@acme.com",
		"first_name": "John",
		"company":    "Acme",
	}

	// Variant 0 = base
	p := BuildEmailForSend(seq, 1, 0, lead, "anders@x.com")
	if p.Subject != "Hi John" {
		t.Errorf("expected 'Hi John', got %q", p.Subject)
	}
	if p.Body != "Hello John at Acme" {
		t.Errorf("expected rendered body, got %q", p.Body)
	}
	if p.FromName != "Anders" {
		t.Errorf("expected from_name 'Anders', got %q", p.FromName)
	}

	// Variant 1 = first variant
	p = BuildEmailForSend(seq, 1, 1, lead, "anders@x.com")
	if p.Subject != "Acme intro" {
		t.Errorf("expected 'Acme intro', got %q", p.Subject)
	}
	if p.Body != "Alt body for John" {
		t.Errorf("expected rendered variant body, got %q", p.Body)
	}
}

func TestBuildEmailForSend_StepNotFound(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Subject: "Hi", Body: "Hello"},
		},
	}

	p := BuildEmailForSend(seq, 99, 0, map[string]string{"email": "x@x.com"}, "a@x.com")
	if p.ToEmail != "" {
		t.Error("expected empty EmailParams for missing step")
	}
}

func TestPrepareFollowUp(t *testing.T) {
	p := EmailParams{
		FromEmail: "anders@x.com",
		ToEmail:   "john@acme.com",
		Body:      "Following up...",
	}

	PrepareFollowUp(&p, "<abc@gmail.com>", "thread-123", "Quick question about Acme")

	if p.InReplyTo != "<abc@gmail.com>" {
		t.Errorf("expected InReplyTo '<abc@gmail.com>', got %q", p.InReplyTo)
	}
	if p.References != "<abc@gmail.com>" {
		t.Errorf("expected References '<abc@gmail.com>', got %q", p.References)
	}
	if p.ThreadID != "thread-123" {
		t.Errorf("expected ThreadID 'thread-123', got %q", p.ThreadID)
	}
	if p.Subject != "Re: Quick question about Acme" {
		t.Errorf("expected 'Re: Quick question about Acme', got %q", p.Subject)
	}
}

func TestPrepareFollowUp_AlreadyHasRe(t *testing.T) {
	p := EmailParams{
		Subject: "Re: Already has prefix",
	}

	PrepareFollowUp(&p, "<abc@gmail.com>", "t-1", "Original")

	// Should not double-prefix
	if p.Subject != "Re: Already has prefix" {
		t.Errorf("should not double Re: prefix, got %q", p.Subject)
	}
}

func TestMockGWS_SendEmail(t *testing.T) {
	mock := &MockGWS{}

	msgID, threadID, err := mock.SendEmail("a@x.com", "to@x.com", "raw-msg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msgID == "" || threadID == "" {
		t.Error("expected non-empty msgID and threadID")
	}

	if len(mock.SentEmails) != 1 {
		t.Fatalf("expected 1 sent email, got %d", len(mock.SentEmails))
	}
	if mock.SentEmails[0].Account != "a@x.com" {
		t.Errorf("expected account a@x.com, got %s", mock.SentEmails[0].Account)
	}
	if mock.SentEmails[0].To != "to@x.com" {
		t.Errorf("expected to to@x.com, got %s", mock.SentEmails[0].To)
	}
}

func TestPlainTextToHTML(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello", "<div>Hello</div>"},
		{"Line 1\nLine 2", "<div>Line 1<br>Line 2</div>"},
		{"Para 1\n\nPara 2", "<div>Para 1<br><br>Para 2</div>"},
		{"Hi\n\nMiddle\n\nBye", "<div>Hi<br><br>Middle<br><br>Bye</div>"},
		{"", "<div></div>"},
		{"Has <html> & \"quotes\"", "<div>Has &lt;html&gt; &amp; \"quotes\"</div>"},
		{"\n\nLeading whitespace\n\n", "<div>Leading whitespace</div>"},       // trimmed
		{"Windows\r\nLine\r\nEndings", "<div>Windows<br>Line<br>Endings</div>"}, // CRLF
		{"Sign off\nAnders\nCompany", "<div>Sign off<br>Anders<br>Company</div>"},
	}

	for _, tt := range tests {
		got := plainTextToHTML(tt.input)
		if got != tt.expected {
			t.Errorf("plainTextToHTML(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMockGWS_SendError(t *testing.T) {
	mock := &MockGWS{
		SendError: fmt.Errorf("auth expired"),
	}

	_, _, err := mock.SendEmail("a@x.com", "to@x.com", "raw-msg")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auth expired") {
		t.Errorf("expected auth expired error, got: %v", err)
	}
}

func TestMockGWS_MessageIDValidation(t *testing.T) {
	// Simulate gws returning empty IDs
	mock := &MockGWS{
		SendMsgID:    "",
		SendThreadID: "",
	}

	// MockGWS defaults to generating IDs — this tests the real GWSCLI behavior
	// which checks for empty IDs. Here we test the mock works correctly.
	msgID, _, err := mock.SendEmail("a@x.com", "to@x.com", "raw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Mock auto-generates IDs when fields are empty
	if msgID == "" {
		t.Error("mock should auto-generate msgID")
	}
}

func TestBuildRawMessage_ListUnsubscribeHeaders(t *testing.T) {
	raw := BuildRawMessage(EmailParams{
		FromEmail:          "anders@example.com",
		ToEmail:            "john@acme.com",
		Subject:            "Quick question",
		Body:               "Hi John",
		UnsubscribeEmail:   "anders@example.com",
		UnsubscribeSubject: "Unsubscribe",
	})

	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decoding raw message: %v", err)
	}
	msg := string(decoded)

	if !strings.Contains(msg, "List-Unsubscribe: <mailto:anders@example.com?subject=Unsubscribe>") {
		t.Error("missing or incorrect List-Unsubscribe header")
	}
	if !strings.Contains(msg, "List-Unsubscribe-Post: List-Unsubscribe=One-Click") {
		t.Error("missing List-Unsubscribe-Post header")
	}
}

func TestBuildRawMessage_NoUnsubscribeWhenEmpty(t *testing.T) {
	raw := BuildRawMessage(EmailParams{
		FromEmail: "anders@example.com",
		ToEmail:   "john@acme.com",
		Subject:   "Hi",
		Body:      "Hello",
	})

	decoded, _ := base64.URLEncoding.DecodeString(raw)
	msg := string(decoded)

	if strings.Contains(msg, "List-Unsubscribe") {
		t.Error("should not have List-Unsubscribe when UnsubscribeEmail is empty")
	}
}
