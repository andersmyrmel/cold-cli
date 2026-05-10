package internal

import (
	"strings"
	"testing"
)

func TestEmailDisplayBodyStripsGmailQuotedThread(t *testing.T) {
	raw := `got it. article looks perfect.
here's the published link:
https://example.com/post

On Thu, Apr 30, 2026 at 7:00 PM Anders From ProductLair <anders@productlair.com> wrote:
> Hi Trevor,
>
> Payment is done.
>
> Here's the draft:
> https://docs.google.com/document/d/example`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
		Snippet:   "got it. article looks perfect.",
	})

	want := "got it. article looks perfect.\nhere's the published link:\nhttps://example.com/post"
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyStripsWrappedGmailQuotedThread(t *testing.T) {
	raw := `Awesome, thanks!

On Thu, Apr 30, 2026 at 7:28 PM Anders From ProductLair <
anders@productlair.com> wrote:
> Hi Trevor,
> Thanks, looks good on my end.`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
	})

	want := "Awesome, thanks!"
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyStripsOutlookQuotedThread(t *testing.T) {
	raw := `Hi Anders,

Please go ahead and send the article.

Best,
Marcia

From: Anders <anders@example.com>
Sent: Tuesday, May 5, 2026 9:14 AM
To: Marcia <marcia@example.com>
Subject: camp vehicle safety article

Hi Marcia,

I wanted to ask about a guest article.`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		TextBody:  raw,
	})

	want := "Hi Anders,\n\nPlease go ahead and send the article.\n\nBest,\nMarcia"
	if got != want {
		t.Fatalf("unexpected display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayBodyKeepsScheduledOutboundBody(t *testing.T) {
	raw := `Hi Trevor,

Thanks, looks good on my end.

On Thu, Apr 30, 2026 at 1:26 PM Trevor <trevor@example.com> wrote:
> got it.`

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
	raw := `Hi Trevor,

Payment is done.

On Thu, Apr 30, 2026 at 12:25 PM Trevor <trevor@example.com> wrote:
> Yes, this is correct.
> Looking forward to it!`

	got := emailDisplayBody(EmailMessage{
		Direction: EmailMessageDirectionOutbound,
		Type:      EmailMessageTypeManualReply,
		TextBody:  raw,
	})

	want := "Hi Trevor,\n\nPayment is done."
	if got != want {
		t.Fatalf("unexpected manual reply display body:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestEmailDisplayHTMLKeepsBasicFormattingAndLinks(t *testing.T) {
	raw := `<div dir="ltr">Regards,<p><strong>Josh McCann</strong><br><i>@monopolymccann</i></p>
<p>📧 <strong>Email:</strong> <a href="mailto:monopolymccnn@gmail.com" target="_blank">monopolymccnn@gmail.com</a><br>
📱 <strong>Instagram:</strong> <a rel="noopener" href="https://instagram.com/monopolymccann" target="_blank">@monopolymccann</a></p></div>`

	got := emailDisplayHTML(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		Type:      EmailMessageTypeReply,
		HTMLBody:  raw,
	})

	for _, want := range []string{
		"<strong>Josh McCann</strong>",
		"<i>@monopolymccann</i>",
		`href="mailto:monopolymccnn@gmail.com"`,
		`href="https://instagram.com/monopolymccann"`,
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
	raw := `<div dir="ltr"><p>Hi there,</p><p>Just jumping in.</p></div>
<script>alert("x")</script>
<img src="https://tracker.example/pixel.png">
<div class="gmail_quote gmail_quote_container">
  <div class="gmail_attr">On Thu, Josh wrote:<br></div>
  <blockquote><div dir="ltr">Old quoted thread</div></blockquote>
</div>`

	got := emailDisplayHTML(EmailMessage{
		Direction: EmailMessageDirectionInbound,
		Type:      EmailMessageTypeReply,
		HTMLBody:  raw,
	})

	if !strings.Contains(got, "Just jumping in.") {
		t.Fatalf("expected new body content, got %q", got)
	}
	for _, blocked := range []string{"Old quoted thread", "<script", "<img", "gmail_quote", "class="} {
		if strings.Contains(got, blocked) {
			t.Fatalf("expected %q to be stripped from display html, got %q", blocked, got)
		}
	}
}
