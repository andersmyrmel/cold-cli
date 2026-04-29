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

	SendError        error
	SendMsgID        string
	SendThreadID     string
	SendRFCMessageID string
	ListError        error
	GetError         error
	ListCalls        []MockListCall
}

type MockSentEmail struct {
	Account  string
	To       string
	RawMsg   string
	ThreadID string
}

type MockListCall struct {
	Account          string
	Query            string
	IncludeSpamTrash bool
}

type MockSMTPEmailSender struct {
	SentEmails []MockSMTPSentEmail
	SendError  error
	MessageID  string
	ThreadID   string
}

type MockSMTPSentEmail struct {
	Account Account
	Params  EmailParams
}

func (m *MockGWS) SendEmail(account, to, rawMsg, threadID string) (string, string, error) {
	if m.SendError != nil {
		return "", "", m.SendError
	}
	m.SentEmails = append(m.SentEmails, MockSentEmail{
		Account:  account,
		To:       to,
		RawMsg:   rawMsg,
		ThreadID: threadID,
	})

	msgID := m.SendMsgID
	if msgID == "" {
		msgID = fmt.Sprintf("msg-%d", len(m.SentEmails))
	}
	sentThreadID := m.SendThreadID
	if sentThreadID == "" {
		sentThreadID = fmt.Sprintf("thread-%d", len(m.SentEmails))
	}

	return msgID, sentThreadID, nil
}

func (m *MockGWS) ListMessages(account, query string, includeSpamTrash ...bool) ([]GWSMessage, error) {
	include := len(includeSpamTrash) > 0 && includeSpamTrash[0]
	m.ListCalls = append(m.ListCalls, MockListCall{
		Account:          account,
		Query:            query,
		IncludeSpamTrash: include,
	})
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
	rfcMessageID := m.SendRFCMessageID
	if rfcMessageID == "" {
		rfcMessageID = fmt.Sprintf("<%s@mock.local>", msgID)
	}
	return &GWSMessage{
		ID:       msgID,
		ThreadID: m.SendThreadID,
		Headers: map[string]string{
			"Message-ID": rfcMessageID,
		},
	}, nil
}

func (m *MockSMTPEmailSender) SendEmail(account Account, params EmailParams) (string, string, error) {
	if m.SendError != nil {
		return "", "", m.SendError
	}
	m.SentEmails = append(m.SentEmails, MockSMTPSentEmail{
		Account: account,
		Params:  params,
	})

	messageID := m.MessageID
	if messageID == "" {
		messageID = fmt.Sprintf("<smtp-%d@example.com>", len(m.SentEmails))
	}
	threadID := m.ThreadID
	if threadID == "" {
		threadID = messageID
	}
	return messageID, threadID, nil
}

// failFirstMockGWS fails the first SendEmail call, succeeds after.
type failFirstMockGWS struct {
	callCount int
}

func (m *failFirstMockGWS) SendEmail(account, to, rawMsg, threadID string) (string, string, error) {
	m.callCount++
	if m.callCount == 1 {
		return "", "", fmt.Errorf("simulated gws failure")
	}
	return fmt.Sprintf("msg-%d", m.callCount), fmt.Sprintf("thread-%d", m.callCount), nil
}

func (m *failFirstMockGWS) ListMessages(account, query string, includeSpamTrash ...bool) ([]GWSMessage, error) {
	return nil, nil
}

func (m *failFirstMockGWS) GetMessage(account, msgID string) (*GWSMessage, error) {
	return nil, fmt.Errorf("not found")
}
