package internal

import (
	"html"
	"strings"
)

func emailDisplayBody(msg EmailMessage) string {
	body := msg.TextBody
	if strings.TrimSpace(body) == "" {
		body = msg.Snippet
	}
	body = normalizeEmailBodyText(html.UnescapeString(body))
	if body == "" {
		return ""
	}

	if msg.Direction == EmailMessageDirectionOutbound && msg.Type != EmailMessageTypeManualReply {
		return body
	}

	displayBody := stripQuotedEmailText(body)
	if displayBody == "" && strings.TrimSpace(msg.Snippet) != "" {
		return normalizeEmailBodyText(html.UnescapeString(msg.Snippet))
	}
	return displayBody
}

func normalizeEmailBodyText(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	return trimAndCollapseBlankLines(lines)
}

func stripQuotedEmailText(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		if isQuotedEmailBoundary(lines, i) {
			break
		}
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if isForwardedMessageBoundary(lower) || isOutlookSeparator(lower) {
			if hasNonBlankLine(kept) {
				break
			}
			continue
		}

		kept = append(kept, line)
	}

	return trimAndCollapseBlankLines(kept)
}

func isQuotedEmailBoundary(lines []string, index int) bool {
	trimmed := strings.TrimSpace(lines[index])
	lower := strings.ToLower(trimmed)
	if trimmed == "" {
		return false
	}

	if strings.HasPrefix(lower, "on ") && strings.HasSuffix(lower, "wrote:") {
		return true
	}
	if strings.Contains(lower, " wrote:") && strings.HasPrefix(lower, "on ") {
		return true
	}
	if isWrappedGmailQuoteHeader(lines, index) {
		return true
	}
	if strings.Contains(lower, "original message") && strings.Contains(lower, "-----") {
		return true
	}
	if isOutlookHeaderBlock(lines, index) {
		return true
	}

	return false
}

func isWrappedGmailQuoteHeader(lines []string, index int) bool {
	lower := strings.ToLower(strings.TrimSpace(lines[index]))
	if !strings.HasPrefix(lower, "on ") {
		return false
	}

	limit := index + 4
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := index + 1; i < limit; i++ {
		line := strings.ToLower(strings.TrimSpace(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "wrote:") || strings.Contains(line, " wrote:") {
			return true
		}
	}
	return false
}

func isOutlookHeaderBlock(lines []string, index int) bool {
	lower := strings.ToLower(strings.TrimSpace(lines[index]))
	if !strings.HasPrefix(lower, "from:") {
		return false
	}

	sawDateLikeLine := false
	sawRecipientOrSubject := false
	limit := index + 6
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := index + 1; i < limit; i++ {
		line := strings.ToLower(strings.TrimSpace(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "sent:") || strings.HasPrefix(line, "date:") {
			sawDateLikeLine = true
			continue
		}
		if strings.HasPrefix(line, "to:") || strings.HasPrefix(line, "subject:") || strings.HasPrefix(line, "cc:") {
			sawRecipientOrSubject = true
		}
	}

	return sawDateLikeLine && sawRecipientOrSubject
}

func isForwardedMessageBoundary(lower string) bool {
	return strings.Contains(lower, "forwarded message") && strings.Contains(lower, "-")
}

func isOutlookSeparator(lower string) bool {
	if len(lower) < 8 {
		return false
	}
	return strings.Trim(lower, "_- ") == ""
}

func hasNonBlankLine(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

func trimAndCollapseBlankLines(lines []string) string {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}

	var out []string
	blank := false
	for _, line := range lines[start:end] {
		if strings.TrimSpace(line) == "" {
			if blank {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, strings.TrimRight(line, " \t"))
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}
