package internal

import (
	"encoding/base64"
	"fmt"
	"mime"
	"net/url"
	"strings"
)

// EmailParams holds everything needed to construct and send an email.
type EmailParams struct {
	FromName  string
	FromEmail string
	ToEmail   string
	Subject   string
	Body      string

	// For follow-ups (step 2+)
	InReplyTo string // Message-ID of the previous step
	References string // same as InReplyTo for simple chains
	ThreadID  string // Gmail thread ID for threading

	// Unsubscribe
	UnsubscribeEmail   string // mailto address for List-Unsubscribe header
	UnsubscribeSubject string // subject for the mailto unsubscribe
}

// BuildRawMessage constructs an RFC 2822 message and returns it as a base64url-encoded string.
func BuildRawMessage(p EmailParams) string {
	var msg strings.Builder

	// From header
	if p.FromName != "" {
		msg.WriteString(fmt.Sprintf("From: %s <%s>\r\n", p.FromName, p.FromEmail))
	} else {
		msg.WriteString(fmt.Sprintf("From: %s\r\n", p.FromEmail))
	}

	msg.WriteString(fmt.Sprintf("To: %s\r\n", p.ToEmail))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeSubject(p.Subject)))

	// Threading headers for follow-ups
	if p.InReplyTo != "" {
		msg.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", p.InReplyTo))
		refs := p.References
		if refs == "" {
			refs = p.InReplyTo
		}
		msg.WriteString(fmt.Sprintf("References: %s\r\n", refs))
	}

	// List-Unsubscribe headers (required by Gmail/Yahoo for bulk senders)
	if p.UnsubscribeEmail != "" {
		subj := url.QueryEscape(p.UnsubscribeSubject)
		msg.WriteString(fmt.Sprintf("List-Unsubscribe: <mailto:%s?subject=%s>\r\n", p.UnsubscribeEmail, subj))
		msg.WriteString("List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
	}

	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(plainTextToHTML(p.Body))

	return base64.URLEncoding.EncodeToString([]byte(msg.String()))
}

// BuildEmailForSend constructs the full email for a scheduled send, applying template rendering
// and selecting the correct variant.
func BuildEmailForSend(
	seq *Sequence,
	stepNumber int,
	variantIndex int,
	lead map[string]string,
	fromEmail string,
) EmailParams {
	// Find the step
	var step *SequenceStep
	for i := range seq.Steps {
		if seq.Steps[i].Step == stepNumber {
			step = &seq.Steps[i]
			break
		}
	}
	if step == nil {
		return EmailParams{}
	}

	// Select subject and body based on variant
	subject := step.Subject
	body := step.Body

	if variantIndex > 0 && variantIndex <= len(step.Variants) {
		v := step.Variants[variantIndex-1]
		if v.Subject != "" {
			subject = v.Subject
		}
		if v.Body != "" {
			body = v.Body
		}
	}

	// Render templates
	subject = RenderTemplate(subject, lead)
	body = RenderTemplate(body, lead)

	return EmailParams{
		FromName:  seq.Defaults.FromName,
		FromEmail: fromEmail,
		ToEmail:   lead["email"],
		Subject:   subject,
		Body:      body,
	}
}

// plainTextToHTML converts plain text body to minimal HTML.
// Double newlines become <br><br> (paragraphs), single newlines become <br>.
// No CSS, styles, or formatting — looks like a normal hand-typed email.
func plainTextToHTML(text string) string {
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)

	// Escape HTML entities
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// Convert newlines to <br>
	text = strings.ReplaceAll(text, "\n", "<br>")

	return "<div>" + text + "</div>"
}

// encodeSubject MIME-encodes a subject line if it contains non-ASCII characters.
func encodeSubject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// PrepareFollowUp adds threading headers to an EmailParams for step 2+.
func PrepareFollowUp(p *EmailParams, parentMessageID, threadID, originalSubject string) {
	p.InReplyTo = parentMessageID
	p.References = parentMessageID
	p.ThreadID = threadID

	// Follow-ups use "Re: <original subject>" if no subject specified
	if p.Subject == "" {
		p.Subject = "Re: " + originalSubject
	} else if !strings.HasPrefix(p.Subject, "Re: ") {
		p.Subject = "Re: " + p.Subject
	}
}
