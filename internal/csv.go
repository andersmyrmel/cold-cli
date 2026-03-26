package internal

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"
)

// UTF-8 BOM bytes
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// LeadRecord represents a parsed CSV row with all fields available by header name.
type LeadRecord struct {
	Fields map[string]string
}

// ParseLeadsCSV reads a CSV file and returns parsed lead records.
// Headers are normalized to lowercase with spaces replaced by underscores.
// Strips UTF-8 BOM if present.
func ParseLeadsCSV(path string) ([]LeadRecord, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CSV: %w", err)
	}

	return ParseLeadsCSVFromReader(bytes.NewReader(data))
}

// ParseLeadsCSVFromReader parses leads CSV from a reader.
// Returns the lead records and the normalized header names.
func ParseLeadsCSVFromReader(r io.Reader) ([]LeadRecord, []string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("reading CSV data: %w", err)
	}

	// Strip UTF-8 BOM
	data = bytes.TrimPrefix(data, utf8BOM)

	reader := csv.NewReader(bytes.NewReader(data))
	reader.TrimLeadingSpace = true

	// Read header row
	headers, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("reading CSV header: %w", err)
	}

	// Normalize headers: lowercase, spaces → underscores, trim
	for i, h := range headers {
		headers[i] = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(h, " ", "_")))
	}

	// Apply field name aliases (e.g., "name" → "first_name")
	canonicalPresent := make(map[string]bool, len(headers))
	for _, h := range headers {
		canonicalPresent[h] = true
	}
	for i, h := range headers {
		if canonical := ResolveAlias(h); canonical != h && !canonicalPresent[canonical] {
			headers[i] = canonical
			canonicalPresent[canonical] = true
		}
	}

	// Validate email column exists
	hasEmail := false
	for _, h := range headers {
		if h == "email" {
			hasEmail = true
			break
		}
	}
	if !hasEmail {
		return nil, nil, fmt.Errorf("CSV missing required 'email' column (found: %s)", strings.Join(headers, ", "))
	}

	// Read data rows
	var records []LeadRecord
	lineNum := 1 // 1-indexed, header is line 1
	for {
		lineNum++
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading CSV line %d: %w", lineNum, err)
		}

		if len(row) != len(headers) {
			return nil, nil, fmt.Errorf("CSV line %d: expected %d columns, got %d", lineNum, len(headers), len(row))
		}

		fields := make(map[string]string, len(headers))
		for i, h := range headers {
			fields[h] = strings.TrimSpace(row[i])
		}

		// Validate email
		email := fields["email"]
		if email == "" {
			return nil, nil, fmt.Errorf("CSV line %d: empty email", lineNum)
		}
		if _, err := mail.ParseAddress(email); err != nil {
			return nil, nil, fmt.Errorf("CSV line %d: invalid email %q: %w", lineNum, email, err)
		}

		records = append(records, LeadRecord{Fields: fields})
	}

	if len(records) == 0 {
		return nil, nil, fmt.Errorf("CSV has no data rows")
	}

	return records, headers, nil
}

// ExtractDomain returns the domain part of an email address.
func ExtractDomain(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}
