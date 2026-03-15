package internal

import (
	"database/sql"
	"fmt"
	"testing"
)

// testDB creates an in-memory SQLite database with the full schema for testing.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// MockGWS is a test implementation that records calls and returns canned responses.
type MockGWS struct {
	SentEmails    []MockSentEmail
	InboxMessages []GWSMessage

	SendError    error
	SendMsgID    string
	SendThreadID string
	ListError    error
	GetError     error
}

type MockSentEmail struct {
	Account string
	To      string
	RawMsg  string
}

func (m *MockGWS) SendEmail(account, to, rawMsg string) (string, string, error) {
	if m.SendError != nil {
		return "", "", m.SendError
	}
	m.SentEmails = append(m.SentEmails, MockSentEmail{
		Account: account,
		To:      to,
		RawMsg:  rawMsg,
	})

	msgID := m.SendMsgID
	if msgID == "" {
		msgID = fmt.Sprintf("msg-%d", len(m.SentEmails))
	}
	threadID := m.SendThreadID
	if threadID == "" {
		threadID = fmt.Sprintf("thread-%d", len(m.SentEmails))
	}

	return msgID, threadID, nil
}

func (m *MockGWS) ListMessages(account, query string) ([]GWSMessage, error) {
	if m.ListError != nil {
		return nil, m.ListError
	}
	return m.InboxMessages, nil
}

func (m *MockGWS) GetMessage(account, msgID string) (*GWSMessage, error) {
	if m.GetError != nil {
		return nil, m.GetError
	}
	for _, msg := range m.InboxMessages {
		if msg.ID == msgID {
			return &msg, nil
		}
	}
	return nil, fmt.Errorf("message %s not found", msgID)
}

// failFirstMockGWS fails the first SendEmail call, succeeds after.
type failFirstMockGWS struct {
	callCount int
}

func (m *failFirstMockGWS) SendEmail(account, to, rawMsg string) (string, string, error) {
	m.callCount++
	if m.callCount == 1 {
		return "", "", fmt.Errorf("simulated gws failure")
	}
	return fmt.Sprintf("msg-%d", m.callCount), fmt.Sprintf("thread-%d", m.callCount), nil
}

func (m *failFirstMockGWS) ListMessages(account, query string) ([]GWSMessage, error) {
	return nil, nil
}

func (m *failFirstMockGWS) GetMessage(account, msgID string) (*GWSMessage, error) {
	return nil, fmt.Errorf("not found")
}
