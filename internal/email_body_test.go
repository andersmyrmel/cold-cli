package internal

import (
	"strings"
	"testing"
)

func TestEmailDisplayBodyStripsGmailQuotedThread(t *testing.T) {
	raw := `The sample loaded successfully.
Here is the sample link:
https://example.com/sample

On Mon, Jan 5, 2026 at 7:00 PM Alex Example <alex@example.com> wrote:
> Hi Taylor,
>
> The sample is ready.
>
> Sample document:
> https://docs.google.com/document/d/example`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
		Snippet:   "The sample loaded successfully.",
	})

	want := "The sample loaded successfully.\nHere is the sample link:\nhttps://example.com/sample"
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyStripsWrappedGmailQuotedThread(t *testing.T) {
	raw := `Thanks for checking.

On Mon, Jan 5, 2026 at 7:28 PM Alex Example <
alex@example.com> wrote:
> Hi Taylor,
> The sample loaded correctly.`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
	})

	want := "Thanks for checking."
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyStripsOutlookQuotedThread(t *testing.T) {
	raw := `Hi Alex,

Please send the sample file.

Best,
Jordan

From: Alex <alex@example.com>
Sent: Tuesday, January 6, 2026 9:14 AM
To: Jordan <jordan@example.com>
Subject: sample file

Hi Jordan,

Could you review the sample?`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
	})

	want := "Hi Alex,\n\nPlease send the sample file.\n\nBest,\nJordan"
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyKeepsScheduledOutboundBody(t *testing.T) {
	raw := `Hi Taylor,

The sample loaded correctly.

On Mon, Jan 5, 2026 at 1:26 PM Taylor <taylor@example.com> wrote:
> Received the sample.`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionOutbound,
		Type:      EmailMessageTypeSent,
		TextBody:  raw,
	})

	if got != raw {
		t.Fatalf("scheduled outbound display body should keep sent body exactly, got %q", got)
	}
}

func TestEmailDisplayBodyStripsManualOutboundQuotedThread(t *testing.T) {
	raw := `Hi Taylor,

The sample is ready.

On Mon, Jan 5, 2026 at 12:25 PM Taylor <taylor@example.com> wrote:
> The sample opens correctly.
> Thanks for checking it.`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionOutbound,
		Type:      EmailMessageTypeManualReply,
		TextBody:  raw,
	})

	want := "Hi Taylor,\n\nThe sample is ready."
	if got != want {
		t.Fatalf("unexpected manual reply display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayHTMLKeepsBasicFormattingAndLinks(t *testing.T) {
	raw := `<div dir="ltr">Regards,<p><strong>Jamie Example</strong><br><i>@examplehandle</i></p>
<p><strong>Email:</strong> <a href="mailto:hello@example.com" target="_blank">hello@example.com</a><br>
<strong>Profile:</strong> <a rel="noopener" href="https://example.com/profile" target="_blank">@examplehandle</a></p></div>`

	got := emailDisplayHTML(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		Type:      EmailMessageTypeReply,
		HTMLBody:  raw,
	})

	for _, want := range []string{
		"<strong>Jamie Example</strong>",
		"<i>@examplehandle</i>",
		`href="mailto:hello@example.com"`,
		`href="https://example.com/profile"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected display html to contain %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "target=\"_blank\"><") {
		t.Fatalf("expected target attributes to be normalized with rel, got %q", got)
	}
}

func TestEmailDisplayHTMLStripsQuotedThreadAndUnsafeMarkup(t *testing.T) {
	raw := `<div dir="ltr"><p>Hi there,</p><p>Here is my update.</p></div>
<script>alert("x")</script>
<img src="https://tracker.example/pixel.png">
<div class="gmail_quote gmail_quote_container">
  <div class="gmail_attr">On Thu, Jamie wrote:<br></div>
  <blockquote><div dir="ltr">Old quoted thread</div></blockquote>
</div>`

	got := emailDisplayHTML(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		Type:      EmailMessageTypeReply,
		HTMLBody:  raw,
	})

	if !strings.Contains(got, "Here is my update.") {
		t.Fatalf("expected new body content, got %q", got)
	}
	for _, blocked := range []string{"Old quoted thread", "<script", "<img", "gmail_quote", "class="} {
		if strings.Contains(got, blocked) {
			t.Fatalf("expected %q to be stripped from display html, got %q", blocked, got)
		}
	}
}
