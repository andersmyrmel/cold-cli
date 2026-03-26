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
			"{{unknown}} stays", // RenderTemplate leaves unmatched; StripUnresolved handles removal
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

func TestRenderTemplate_AliasResolution(t *testing.T) {
	// {{name}} should resolve to first_name value via alias
	fields := map[string]string{"first_name": "Mike", "last_name": "Smith"}

	got := RenderTemplate("Hi {{name}}", fields)
	if got != "Hi Mike" {
		t.Errorf("expected 'Hi Mike', got %q", got)
	}

	// {{firstname}} alias
	got = RenderTemplate("Hi {{firstname}}", fields)
	if got != "Hi Mike" {
		t.Errorf("expected 'Hi Mike' for {{firstname}}, got %q", got)
	}

	// {{last}} alias
	got = RenderTemplate("Dear {{last}}", fields)
	if got != "Dear Smith" {
		t.Errorf("expected 'Dear Smith' for {{last}}, got %q", got)
	}

	// Alias should NOT override an explicit field with the same name
	fieldsWithName := map[string]string{"first_name": "Mike", "name": "Custom Name"}
	got = RenderTemplate("Hi {{name}}", fieldsWithName)
	if got != "Hi Custom Name" {
		t.Errorf("explicit 'name' field should win over alias, got %q", got)
	}
}

func TestResolveAlias(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"name", "first_name"},
		{"firstname", "first_name"},
		{"first", "first_name"},
		{"lastname", "last_name"},
		{"last", "last_name"},
		{"first_name", "first_name"}, // canonical stays unchanged
		{"email", "email"},           // not an alias
		{"custom", "custom"},         // unknown stays unchanged
	}

	for _, tt := range tests {
		got := ResolveAlias(tt.input)
		if got != tt.expected {
			t.Errorf("ResolveAlias(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "a", 1},
		{"kitten", "sitting", 3},
		{"first_name", "fist_name", 1},
		{"company", "company", 0},
		{"first_name", "first_name", 0},
	}

	for _, tt := range tests {
		got := levenshteinDistance(tt.a, tt.b)
		if got != tt.expected {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.expected)
		}
	}
}

func TestSuggestField(t *testing.T) {
	available := []string{"email", "first_name", "last_name", "company", "domain"}

	// Close match
	got := SuggestField("fist_name", available)
	if got != "first_name" {
		t.Errorf("expected suggestion 'first_name' for 'fist_name', got %q", got)
	}

	// Close match for company
	got = SuggestField("compnay", available)
	if got != "company" {
		t.Errorf("expected suggestion 'company' for 'compnay', got %q", got)
	}

	// Too far away — no suggestion
	got = SuggestField("zzzzzzzzzzz", available)
	if got != "" {
		t.Errorf("expected no suggestion for 'zzzzzzzzzzz', got %q", got)
	}
}

func TestStripUnresolved(t *testing.T) {
	tests := []struct {
		input          string
		expectedOutput string
		expectedVars   []string
	}{
		// No placeholders — unchanged
		{"Hello world", "Hello world", nil},
		// Single unresolved variable stripped
		{"Hi {{first_name}},", "Hi ,", []string{"first_name"}},
		// Variable with surrounding spaces — double space collapsed
		{"Hi {{name}}, welcome", "Hi , welcome", []string{"name"}},
		// Variable between words — double space collapsed
		{"data for your {{article_title}} blog", "data for your blog", []string{"article_title"}},
		// Multiple unresolved variables
		{"{{greeting}} {{name}}", "", []string{"greeting", "name"}},
		// Duplicate variable names — deduplicated in returned list
		{"{{x}} and {{x}}", "and", []string{"x"}},
		// Empty string
		{"", "", nil},
	}

	for _, tt := range tests {
		got, vars := StripUnresolved(tt.input)
		if got != tt.expectedOutput {
			t.Errorf("StripUnresolved(%q) = %q, want %q", tt.input, got, tt.expectedOutput)
		}
		if len(vars) != len(tt.expectedVars) {
			t.Errorf("StripUnresolved(%q) vars = %v, want %v", tt.input, vars, tt.expectedVars)
			continue
		}
		for i, v := range vars {
			if v != tt.expectedVars[i] {
				t.Errorf("StripUnresolved(%q) vars[%d] = %q, want %q", tt.input, i, v, tt.expectedVars[i])
			}
		}
	}
}
