package internal

import (
	"testing"
	"time"
)

func TestGetAllCampaignStats_Empty(t *testing.T) {
	db := testDB(t)

	stats, err := GetAllCampaignStats(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats, got %d", len(stats))
	}
}

func TestGetAllCampaignStats_WithEvents(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('camp1', 'seq.yml')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('camp2', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")

	now := time.Now().UTC().Format(time.RFC3339)
	// camp1: 2 sent, 1 reply, 1 bounce
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 2, 1, 'sent', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'reply', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 2, 1, 'bounce', 1, ?)", now)

	stats, err := GetAllCampaignStats(db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 campaigns, got %d", len(stats))
	}

	// Find camp1 (order is by created_at DESC, so camp2 may come first)
	var camp1 CampaignStats
	for _, s := range stats {
		if s.Name == "camp1" {
			camp1 = s
		}
	}
	if camp1.Sent != 2 {
		t.Errorf("expected 2 sent, got %d", camp1.Sent)
	}
	if camp1.Replies != 1 {
		t.Errorf("expected 1 reply, got %d", camp1.Replies)
	}
	if camp1.Bounces != 1 {
		t.Errorf("expected 1 bounce, got %d", camp1.Bounces)
	}
}

func TestGetCampaignStepStats(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")

	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 2, 1, 'sent', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 2, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'reply', 2, ?)", now)

	stats, err := GetCampaignStepStats(db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(stats))
	}

	// Step 1: 2 sent, 0 replies
	if stats[0].Step != 1 {
		t.Errorf("expected step 1, got %d", stats[0].Step)
	}
	if stats[0].Sent != 2 {
		t.Errorf("step 1: expected 2 sent, got %d", stats[0].Sent)
	}
	if stats[0].Replies != 0 {
		t.Errorf("step 1: expected 0 replies, got %d", stats[0].Replies)
	}

	// Step 2: 1 sent, 1 reply
	if stats[1].Step != 2 {
		t.Errorf("expected step 2, got %d", stats[1].Step)
	}
	if stats[1].Sent != 1 {
		t.Errorf("step 2: expected 1 sent, got %d", stats[1].Sent)
	}
	if stats[1].Replies != 1 {
		t.Errorf("step 2: expected 1 reply, got %d", stats[1].Replies)
	}
}

func TestGetCampaignStepStats_Empty(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")

	stats, err := GetCampaignStepStats(db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats, got %d", len(stats))
	}
}

func TestGetEventLog(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")

	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp) VALUES (1, 1, 1, 'sent', 1, 'msg-1', ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, message_id, timestamp) VALUES (1, 1, 1, 'reply', 1, 'msg-2', ?)", now)

	// All events
	events, err := GetEventLog(db, "", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Verify fields populated
	if events[0].Campaign != "c1" {
		t.Errorf("expected campaign 'c1', got %q", events[0].Campaign)
	}
	if events[0].LeadEmail != "a@x.com" {
		t.Errorf("expected lead 'a@x.com', got %q", events[0].LeadEmail)
	}
	if events[0].AccountEmail != "sender@x.com" {
		t.Errorf("expected account 'sender@x.com', got %q", events[0].AccountEmail)
	}

	// Filter by campaign
	events, err = GetEventLog(db, "c1", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 events for c1, got %d", len(events))
	}

	// Limit
	events, err = GetEventLog(db, "", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event with limit=1, got %d", len(events))
	}
}

func TestGetEventLog_Empty(t *testing.T) {
	db := testDB(t)

	events, err := GetEventLog(db, "", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestGetCampaignLeadStats(t *testing.T) {
	db := testDB(t)

	db.Exec("INSERT INTO accounts (email) VALUES ('sender@x.com')")
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('a@x.com', 'x.com')")
	db.Exec("INSERT INTO leads (email, domain) VALUES ('b@x.com', 'x.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 1, 'replied')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (1, 2, 'active')")

	now := time.Now().UTC().Format(time.RFC3339)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 1, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'sent', 2, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 1, 1, 'reply', 2, ?)", now)
	db.Exec("INSERT INTO events (campaign_id, lead_id, account_id, type, step_number, timestamp) VALUES (1, 2, 1, 'sent', 1, ?)", now)

	stats, err := GetCampaignLeadStats(db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 leads, got %d", len(stats))
	}

	// a@x.com: replied, 2 steps sent, has reply_at
	var aStats LeadStatsRow
	for _, s := range stats {
		if s.Email == "a@x.com" {
			aStats = s
		}
	}
	if aStats.Status != "replied" {
		t.Errorf("expected status 'replied', got %q", aStats.Status)
	}
	if aStats.StepsSent != 2 {
		t.Errorf("expected 2 steps sent, got %d", aStats.StepsSent)
	}
	if aStats.ReplyAt == nil {
		t.Error("expected reply_at to be set")
	}

	// b@x.com: active, 1 step sent, no reply
	var bStats LeadStatsRow
	for _, s := range stats {
		if s.Email == "b@x.com" {
			bStats = s
		}
	}
	if bStats.Status != "active" {
		t.Errorf("expected status 'active', got %q", bStats.Status)
	}
	if bStats.StepsSent != 1 {
		t.Errorf("expected 1 step sent, got %d", bStats.StepsSent)
	}
	if bStats.ReplyAt != nil {
		t.Error("expected reply_at to be nil")
	}
}

func TestGetCampaignLeadStats_Empty(t *testing.T) {
	db := testDB(t)
	db.Exec("INSERT INTO campaigns (name, sequence_file) VALUES ('c1', 'seq.yml')")

	stats, err := GetCampaignLeadStats(db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats, got %d", len(stats))
	}
}
