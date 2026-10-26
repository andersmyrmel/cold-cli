package internal

import (
	"fmt"
	"testing"
	"time"
)

func zendeskReplyFixture(comment string) GWSMessage {
	return GWSMessage{
		ID: "helpdesk-reply", ThreadID: "thread-helpdesk", InReplyTo: "sent-helpdesk",
		From: "Example Support <support@example.com>", Subject: "[Example] Re: Publisher partnership",
		Snippet: "Your request (431802) has been updated. To add additional comments, reply to this email.",
		TextBody: "Your request (431802) has been updated. To add additional comments, reply to this email.\r\n" +
			"----------------------------------------------\r\n\r\n" +
			"Casey Morris, 14 Oct 2026, 14:26 BST\r\n\r\n" + comment +
			"\r\n\r\n----------------------------------------------\r\n\r\n" +
			"Anders, 12 Oct 2026, 09:15 BST\r\n\r\nOriginal message. To stop these emails, reply Unsubscribe.",
		Headers: map[string]string{"Auto-Submitted": "auto-generated", "X-Mailer": "Zendesk Mailer"},
	}
}

func TestClassifyInboundMessageHelpdeskAndQuotedHistory(t *testing.T) {
	human := zendeskReplyFixture("Hey Anders,\r\nHere are our requirements: active niche blogs with identifiable authors.")
	redirect := zendeskReplyFixture("Hi Anders,\r\nPlease email me over at link-outreach@example.com.\r\nThanks, Casey")
	ack := GWSMessage{
		Subject: "[Request received]", TextBody: "Your request (26300) has been received and is being reviewed by our support staff.",
		Headers: map[string]string{"Auto-Submitted": "auto-generated", "X-Mailer": "Zendesk Mailer"},
	}
	tests := []struct {
		name string
		msg  GWSMessage
		want inboundClassification
	}{
		{"agent requirements", human, inboundClassificationReply},
		{"agent redirects contact", redirect, inboundClassificationReply},
		{"ticket acknowledgement", ack, inboundClassificationAutoReply},
		{"automated comment", zendeskReplyFixture("We have received your request. We'll pick up your ticket as soon as possible."), inboundClassificationAutoReply},
		{"unsubscribe in newest comment", zendeskReplyFixture("Please remove me from your list."), inboundClassificationUnsubscribe},
		{"ordinary greeting", GWSMessage{Subject: "Re: Partnership", TextBody: "Thank you for contacting us. Yes, please send your sites."}, inboundClassificationReply},
		{"support ticket mentioned", GWSMessage{Subject: "Re: Partnership", TextBody: "I reviewed your support ticket. Our budget is $300."}, inboundClassificationReply},
		{"quoted acknowledgement", GWSMessage{Subject: "Re: Partnership", TextBody: "Yes, please send details.\n\nOn Tue, Support wrote:\nThank you for contacting us. Your ticket has been submitted."}, inboundClassificationReply},
		{"quoted unsubscribe footer", GWSMessage{Subject: "Re: Partnership", TextBody: "Yes, please send details.\n\nOn Tue, Anders wrote:\nTo stop these emails, reply Unsubscribe."}, inboundClassificationReply},
		{"stale snippet", GWSMessage{Subject: "Re: Partnership", Snippet: "Your ticket has been submitted.", TextBody: "Yes, please send details."}, inboundClassificationReply},
		{"html quoted acknowledgement", GWSMessage{Subject: "Re: Partnership", HTMLBody: "<p>Yes, please send details.</p><blockquote>Your ticket has been submitted.</blockquote>"}, inboundClassificationReply},
		{"actual generated notification", GWSMessage{TextBody: "Your report is ready.", Headers: map[string]string{"Auto-Submitted": "auto-generated"}}, inboundClassificationAutoReply},
		{"actual vacation reply", GWSMessage{Subject: "Automatic reply", TextBody: "I am away until Monday.", Headers: map[string]string{"Auto-Submitted": "auto-replied"}}, inboundClassificationAutoReply},
		{"ack without headers", GWSMessage{Subject: "Re: Partnership", TextBody: "Your ticket has been submitted. We'll pick up your ticket shortly."}, inboundClassificationAutoReply},
		{"snippet only acknowledgement", GWSMessage{Snippet: "Your request has been received."}, inboundClassificationAutoReply},
		{"auto submitted no with parameters", GWSMessage{TextBody: "Yes, please send details.", Headers: map[string]string{"auto-submitted": "No; owner-email=support@example.com"}}, inboundClassificationReply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyInboundMessage(tt.msg); got != tt.want {
				t.Fatalf("classification = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("provider marker alone does not prove human authorship", func(t *testing.T) {
		msg := human
		msg.TextBody = "Your request (431802) has been updated."
		if got := classifyInboundMessage(msg); got != inboundClassificationAutoReply {
			t.Fatalf("classification = %q, want auto_reply", got)
		}
	})
	t.Run("auto-replied remains authoritative", func(t *testing.T) {
		msg := human
		msg.Headers = map[string]string{"Auto-Submitted": "auto-replied", "X-Mailer": "Zendesk Mailer"}
		if got := classifyInboundMessage(msg); got != inboundClassificationAutoReply {
			t.Fatalf("classification = %q, want auto_reply", got)
		}
	})
	t.Run("empty newest comment is not a human reply", func(t *testing.T) {
		msg := zendeskReplyFixture("")
		if got := classifyInboundMessage(msg); got != inboundClassificationAutoReply {
			t.Fatalf("classification = %q, want auto_reply", got)
		}
	})
}

func TestProcessRepliesHelpdeskStopsFollowupsAndDeduplicates(t *testing.T) {
	for _, provider := range []string{AccountProviderGWS, AccountProviderSMTPIMAP} {
		t.Run(provider, func(t *testing.T) {
			db := setupReplyTestDB(t)
			_, err := db.Exec(`INSERT INTO events (campaign_id,lead_id,account_id,type,step_number,message_id,thread_id)
				VALUES (1,1,1,'sent',1,'sent-helpdesk','thread-helpdesk')`)
			if err != nil {
				t.Fatal(err)
			}
			for step := 2; step <= 3; step++ {
				if _, err := db.Exec(`INSERT INTO scheduled_sends (campaign_id,lead_id,account_id,step_number,send_at,status)
					VALUES (1,1,1,?,'2099-01-01','pending')`, step); err != nil {
					t.Fatal(err)
				}
			}
			msg := zendeskReplyFixture("Please send your sites and prices.")
			msg.Date = time.Date(2026, 10, 14, 13, 26, 23, 0, time.UTC)
			account := Account{ID: 1, Email: "sender@x.com", Provider: provider, Status: "active"}
			poll := func() (int, int, error) {
				if provider == AccountProviderGWS {
					return ProcessReplies(db, &MockGWS{InboxMessages: []GWSMessage{msg}}, []Account{account})
				}
				return ProcessIMAPReplies(db, &MockIMAPMessageLister{Messages: []GWSMessage{msg}}, []Account{account})
			}
			replies, unsubs, err := poll()
			if err != nil || replies != 1 || unsubs != 0 {
				t.Fatalf("first poll: replies=%d unsubs=%d, %v", replies, unsubs, err)
			}
			var status string
			if err := db.QueryRow("SELECT status FROM campaign_leads WHERE campaign_id=1 AND lead_id=1").Scan(&status); err != nil || status != "replied" {
				t.Fatalf("lead status = %q, error = %v", status, err)
			}
			for table, predicate := range map[string]string{
				"events": "type='reply'", "email_messages": "type='reply' AND direction='inbound'", "scheduled_sends": "status='skipped'",
			} {
				var count int
				if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, predicate)).Scan(&count); err != nil {
					t.Fatal(err)
				}
				want := 1
				if table == "scheduled_sends" {
					want = 2
				}
				if count != want {
					t.Fatalf("%s count = %d, want %d", table, count, want)
				}
			}
			replies, unsubs, err = poll()
			if err != nil || replies != 0 || unsubs != 0 {
				t.Fatalf("repeat poll: replies=%d unsubs=%d, %v", replies, unsubs, err)
			}
		})
	}
}
