package internal

import (
	"testing"
)

func TestExtractPlaceholders(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"Hi {{first_name}}", []string{"first_name"}},
		{"{{first_name}} at {{company}}", []string{"first_name", "company"}},
		{"{{first_name}} and {{first_name}}", []string{"first_name"}}, // deduped
		{"no placeholders here", nil},
		{"", nil},
		{"{{a}} {{b}} {{c}}", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		got := ExtractPlaceholders(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("ExtractPlaceholders(%q) = %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i, v := range got {
			if v != tt.expected[i] {
				t.Errorf("ExtractPlaceholders(%q)[%d] = %q, want %q", tt.input, i, v, tt.expected[i])
			}
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	tests := []struct {
		tmpl     string
		fields   map[string]string
		expected string
	}{
		{
			"Hi {{first_name}}, welcome to {{company}}",
			map[string]string{"first_name": "John", "company": "Acme"},
			"Hi John, welcome to Acme",
		},
		{
			"{{first_name}} {{first_name}}", // multiple occurrences
			map[string]string{"first_name": "Jane"},
			"Jane Jane",
		},
		{
			"No placeholders",
			map[string]string{"first_name": "John"},
			"No placeholders",
		},
		{
			"{{unknown}} stays",
			map[string]string{"first_name": "John"},
			"{{unknown}} stays", // unmatched placeholders left as-is
		},
		{
			"",
			map[string]string{},
			"",
		},
	}

	for _, tt := range tests {
		got := RenderTemplate(tt.tmpl, tt.fields)
		if got != tt.expected {
			t.Errorf("RenderTemplate(%q, ...) = %q, want %q", tt.tmpl, got, tt.expected)
		}
	}
}
