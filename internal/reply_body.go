package internal

import (
	"html"
	"regexp"
	"strings"
)

var zendeskUpdatedRequest = regexp.MustCompile(`(?i)your request \([0-9]+\) has been updated\.`)
var zendeskCommentAuthor = regexp.MustCompile(`^[^\n]+, [0-9]{1,2} [A-Za-z]{3} [0-9]{4}, [0-9]{1,2}:[0-9]{2}(?: [A-Za-z0-9:+-]+)?$`)

// inboundClassificationBody excludes old conversation text and prefers the full
// body over the provider's potentially truncated or stale snippet. Zendesk sends
// both agent comments and acknowledgements as auto-generated notifications; only
// a recognizable, nonempty newest comment can relax that header's classification.
func inboundClassificationBody(msg GWSMessage) (body string, zendeskComment bool) {
	body = msg.TextBody
	if strings.TrimSpace(body) == "" && strings.TrimSpace(msg.HTMLBody) != "" {
		body = emailHTMLToText(sanitizeEmailHTML(msg.HTMLBody, true))
	}
	if strings.TrimSpace(body) == "" {
		body = msg.Snippet
	}
	body = normalizeEmailBodyText(html.UnescapeString(body))
	if strings.EqualFold(strings.TrimSpace(headerValue(msg.Headers, "X-Mailer")), "Zendesk Mailer") ||
		strings.TrimSpace(headerValue(msg.Headers, "X-Zendesk-Email-Id")) != "" {
		parts := strings.Split(body, "\n----------------------------------------------\n")
		if len(parts) >= 2 && zendeskUpdatedRequest.MatchString(parts[0]) {
			comment := strings.TrimSpace(parts[1])
			author, text, found := strings.Cut(comment, "\n")
			if found && zendeskCommentAuthor.MatchString(strings.TrimSpace(author)) {
				text = stripQuotedEmailText(text)
				if strings.TrimSpace(text) != "" {
					return text, true
				}
			}
		}
	}
	return stripQuotedEmailText(body), false
}
