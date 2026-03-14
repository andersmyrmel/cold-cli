package internal

import (
	"regexp"
	"strings"
)

var placeholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// ExtractPlaceholders returns all unique {{placeholder}} names from a string.
func ExtractPlaceholders(s string) []string {
	matches := placeholderRe.FindAllStringSubmatch(s, -1)
	seen := map[string]bool{}
	var result []string
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

// RenderTemplate replaces all {{placeholder}} occurrences with values from the fields map.
func RenderTemplate(tmpl string, fields map[string]string) string {
	result := tmpl
	for key, val := range fields {
		result = strings.ReplaceAll(result, "{{"+key+"}}", val)
	}
	return result
}
