package internal

import (
	"html"
	"net/url"
	"strings"

	htmlnode "golang.org/x/net/html"
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

func emailDisplayHTML(msg EmailMessage) string {
	htmlBody := strings.TrimSpace(msg.HTMLBody)
	if htmlBody == "" {
		return ""
	}
	stripQuotes := msg.Direction == EmailMessageDirectionInbound || msg.Type == EmailMessageTypeManualReply
	return sanitizeEmailHTML(htmlBody, stripQuotes)
}

func normalizeEmailBodyText(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	return trimAndCollapseBlankLines(lines)
}

func sanitizeEmailHTML(raw string, stripQuotes bool) string {
	root, err := htmlnode.Parse(strings.NewReader("<!doctype html><html><body>" + raw + "</body></html>"))
	if err != nil {
		return ""
	}

	var out strings.Builder
	writeSanitizedHTML(&out, root, stripQuotes)
	return strings.TrimSpace(out.String())
}

func writeSanitizedHTML(out *strings.Builder, node *htmlnode.Node, stripQuotes bool) {
	switch node.Type {
	case htmlnode.TextNode:
		out.WriteString(html.EscapeString(node.Data))
	case htmlnode.ElementNode:
		tag := strings.ToLower(node.Data)
		if shouldSkipEmailHTMLElement(node, stripQuotes) {
			return
		}
		if isDroppedEmailHTMLTag(tag) {
			return
		}

		sanitizedTag, allowed := sanitizedEmailHTMLTag(tag)
		if !allowed {
			writeSanitizedHTMLChildren(out, node, stripQuotes)
			return
		}

		if sanitizedTag == "br" {
			out.WriteString("<br>")
			return
		}

		out.WriteByte('<')
		out.WriteString(sanitizedTag)
		if sanitizedTag == "a" {
			if href := sanitizedEmailHTMLHref(node); href != "" {
				out.WriteString(` href="`)
				out.WriteString(html.EscapeString(href))
				out.WriteString(`" target="_blank" rel="noopener noreferrer"`)
			}
		}
		out.WriteByte('>')
		writeSanitizedHTMLChildren(out, node, stripQuotes)
		out.WriteString("</")
		out.WriteString(sanitizedTag)
		out.WriteByte('>')
	case htmlnode.DocumentNode:
		writeSanitizedHTMLChildren(out, node, stripQuotes)
	}
}

func writeSanitizedHTMLChildren(out *strings.Builder, node *htmlnode.Node, stripQuotes bool) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		writeSanitizedHTML(out, child, stripQuotes)
	}
}

func shouldSkipEmailHTMLElement(node *htmlnode.Node, stripQuotes bool) bool {
	tag := strings.ToLower(node.Data)
	if !stripQuotes {
		return false
	}
	if tag == "blockquote" {
		return true
	}
	classes := strings.Fields(strings.ToLower(attrValue(node, "class")))
	for _, className := range classes {
		switch className {
		case "gmail_quote", "gmail_attr", "gmail_extra", "yahoo_quoted", "moz-cite-prefix", "protonmail_quote":
			return true
		}
		if strings.Contains(className, "gmail_quote") || strings.Contains(className, "yahoo_quoted") {
			return true
		}
	}
	return false
}

func isDroppedEmailHTMLTag(tag string) bool {
	switch tag {
	case "script", "style", "meta", "link", "title", "head", "iframe", "object", "embed", "form", "input", "button", "img", "svg", "canvas", "video", "audio":
		return true
	default:
		return false
	}
}

func sanitizedEmailHTMLTag(tag string) (string, bool) {
	switch tag {
	case "a", "strong", "b", "em", "i", "u", "s", "p", "br", "div", "span", "ul", "ol", "li", "pre", "code", "table", "thead", "tbody", "tr", "th", "td":
		return tag, true
	default:
		return "", false
	}
}

func sanitizedEmailHTMLHref(node *htmlnode.Node) string {
	href := strings.TrimSpace(attrValue(node, "href"))
	if href == "" {
		return ""
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "mailto":
		return href
	default:
		return ""
	}
}

func attrValue(node *htmlnode.Node, name string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, name) {
			return attr.Val
		}
	}
	return ""
}

func htmlTextContent(node *htmlnode.Node) string {
	var out strings.Builder
	var walk func(*htmlnode.Node)
	walk = func(n *htmlnode.Node) {
		if n.Type == htmlnode.TextNode {
			out.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.TrimSpace(out.String())
}

func emailHTMLToText(raw string) string {
	root, err := htmlnode.Parse(strings.NewReader("<!doctype html><html><body>" + raw + "</body></html>"))
	if err != nil {
		return ""
	}
	text := htmlTextContent(root)
	if text == "" {
		return ""
	}
	return trimAndCollapseBlankLines(strings.Split(text, "\n"))
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
