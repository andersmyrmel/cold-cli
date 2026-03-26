package internal

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseLeadsCSV_Basic(t *testing.T) {
	csv := "email,first_name,company\njohn@acme.com,John,Acme Inc\njane@foo.com,Jane,Foo Corp\n"
	records, headers, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if len(headers) != 3 {
		t.Fatalf("expected 3 headers, got %d", len(headers))
	}

	if records[0].Fields["email"] != "john@acme.com" {
		t.Errorf("expected john@acme.com, got %s", records[0].Fields["email"])
	}
	if records[0].Fields["first_name"] != "John" {
		t.Errorf("expected John, got %s", records[0].Fields["first_name"])
	}
	if records[1].Fields["company"] != "Foo Corp" {
		t.Errorf("expected Foo Corp, got %s", records[1].Fields["company"])
	}
}

func TestParseLeadsCSV_BOMStripping(t *testing.T) {
	// UTF-8 BOM + CSV content
	bom := string(utf8BOM)
	csv := bom + "email,first_name\njohn@acme.com,John\n"
	records, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Fields["email"] != "john@acme.com" {
		t.Errorf("expected john@acme.com, got %s", records[0].Fields["email"])
	}
}

func TestParseLeadsCSV_HeaderNormalization(t *testing.T) {
	csv := "Email,First Name,COMPANY\njohn@acme.com,John,Acme\n"
	_, headers, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"email", "first_name", "company"}
	for i, h := range headers {
		if h != expected[i] {
			t.Errorf("header %d: expected %q, got %q", i, expected[i], h)
		}
	}
}

func TestParseLeadsCSV_MissingEmailColumn(t *testing.T) {
	csv := "first_name,company\nJohn,Acme\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for missing email column")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should mention email column: %v", err)
	}
}

func TestParseLeadsCSV_EmptyEmail(t *testing.T) {
	csv := "email,first_name\n,John\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for empty email")
	}
	if !strings.Contains(err.Error(), "empty email") {
		t.Errorf("error should mention empty email: %v", err)
	}
}

func TestParseLeadsCSV_InvalidEmail(t *testing.T) {
	csv := "email,first_name\nnot-an-email,John\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for invalid email")
	}
	if !strings.Contains(err.Error(), "invalid email") {
		t.Errorf("error should mention invalid email: %v", err)
	}
}

func TestParseLeadsCSV_NoDataRows(t *testing.T) {
	csv := "email,first_name\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for no data rows")
	}
	if !strings.Contains(err.Error(), "no data rows") {
		t.Errorf("error should mention no data rows: %v", err)
	}
}

func TestParseLeadsCSV_ColumnMismatch(t *testing.T) {
	csv := "email,first_name,company\njohn@acme.com,John\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for column count mismatch")
	}
}

func TestParseLeadsCSV_TrimWhitespace(t *testing.T) {
	csv := "email,first_name\n  john@acme.com , John  \n"
	records, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if records[0].Fields["email"] != "john@acme.com" {
		t.Errorf("expected trimmed email, got %q", records[0].Fields["email"])
	}
	if records[0].Fields["first_name"] != "John" {
		t.Errorf("expected trimmed first_name, got %q", records[0].Fields["first_name"])
	}
}

func TestParseLeadsCSV_AliasMapping(t *testing.T) {
	// CSV with "name" column should be mapped to "first_name"
	csv := "email,name,company\njohn@acme.com,John,Acme\n"
	records, headers, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Header should be mapped
	found := false
	for _, h := range headers {
		if h == "first_name" {
			found = true
		}
		if h == "name" {
			t.Error("header 'name' should have been mapped to 'first_name'")
		}
	}
	if !found {
		t.Error("expected 'first_name' in headers after alias mapping")
	}

	// Field values should use the canonical name
	if records[0].Fields["first_name"] != "John" {
		t.Errorf("expected first_name='John', got %q", records[0].Fields["first_name"])
	}
}

func TestParseLeadsCSV_AliasNotAppliedWhenCanonicalExists(t *testing.T) {
	// CSV with both "name" and "first_name" — alias should NOT be applied
	csv := "email,name,first_name\njohn@acme.com,Johnny,John\n"
	records, headers, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both columns should exist
	nameCount := 0
	for _, h := range headers {
		if h == "name" || h == "first_name" {
			nameCount++
		}
	}
	if nameCount != 2 {
		t.Errorf("expected both 'name' and 'first_name' in headers, got %v", headers)
	}
	if records[0].Fields["name"] != "Johnny" {
		t.Errorf("expected name='Johnny', got %q", records[0].Fields["name"])
	}
	if records[0].Fields["first_name"] != "John" {
		t.Errorf("expected first_name='John', got %q", records[0].Fields["first_name"])
	}
}

func TestParseLeadsCSV_LastnameAlias(t *testing.T) {
	csv := "email,lastname\njohn@acme.com,Doe\n"
	records, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if records[0].Fields["last_name"] != "Doe" {
		t.Errorf("expected last_name='Doe', got %q", records[0].Fields["last_name"])
	}
}

func TestParseLeadsCSV_ReservedColumnName(t *testing.T) {
	reserved := []string{"subject", "body", "step", "delay", "variant"}
	for _, name := range reserved {
		csv := fmt.Sprintf("email,%s\njohn@acme.com,value\n", name)
		_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
		if err == nil {
			t.Errorf("expected error for reserved column %q, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), "conflicts with reserved field name") {
			t.Errorf("expected reserved field error for %q, got: %v", name, err)
		}
	}
}

func TestParseLeadsCSV_ReservedColumnNameSuggestion(t *testing.T) {
	csv := "email,subject\njohn@acme.com,hello\n"
	_, _, err := ParseLeadsCSVFromReader(strings.NewReader(csv))
	if err == nil {
		t.Fatal("expected error for reserved 'subject' column")
	}
	if !strings.Contains(err.Error(), "subject_line") {
		t.Errorf("expected rename suggestion 'subject_line', got: %v", err)
	}
}

func TestBuildCustomFieldsJSON(t *testing.T) {
	// Only non-builtin, non-empty fields
	fields := map[string]string{
		"email": "a@x.com", "first_name": "A", "company": "X",
		"slug": "my-slug", "category": "widgets",
	}
	got := BuildCustomFieldsJSON(fields)
	if !strings.Contains(got, `"slug":"my-slug"`) {
		t.Errorf("expected slug in JSON, got %q", got)
	}
	if !strings.Contains(got, `"category":"widgets"`) {
		t.Errorf("expected category in JSON, got %q", got)
	}
	if strings.Contains(got, "email") || strings.Contains(got, "first_name") {
		t.Errorf("builtin fields should not be in custom JSON, got %q", got)
	}
}

func TestBuildCustomFieldsJSON_Empty(t *testing.T) {
	fields := map[string]string{"email": "a@x.com", "first_name": "A"}
	got := BuildCustomFieldsJSON(fields)
	if got != "{}" {
		t.Errorf("expected '{}' for no custom fields, got %q", got)
	}
}

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		email  string
		domain string
	}{
		{"john@acme.com", "acme.com"},
		{"JANE@Foo.COM", "foo.com"},
		{"invalid", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := ExtractDomain(tt.email)
		if got != tt.domain {
			t.Errorf("ExtractDomain(%q) = %q, want %q", tt.email, got, tt.domain)
		}
	}
}
