package internal

import (
	"database/sql"
	"testing"
	"time"
)

const cohortStartSeqYAML = `name: Test
defaults:
  from_name: "Test"
steps:
  - step: 1
    delay: 0
    subject: "Hi {{first_name}}"
    body: "Hello {{first_name}}"
  - step: 2
    delay: 3
    body: "Step 2"
  - step: 3
    delay: 5
    body: "Step 3"
`

type cohortSend struct {
	Step   int
	SendAt time.Time
}

func loadLeadSends(t *testing.T, db *sql.DB, campaignID int64, email string) []cohortSend {
	t.Helper()
	rows, err := db.Query(`SELECT ss.step_number, CAST(ss.send_at AS TEXT)
		FROM scheduled_sends ss JOIN leads l ON l.id = ss.lead_id
		WHERE ss.campaign_id = ? AND l.email = ? ORDER BY ss.step_number`, campaignID, email)
	if err != nil {
		t.Fatalf("loading sends for %s: %v", email, err)
	}
	defer rows.Close()
	var sends []cohortSend
	for rows.Next() {
		var send cohortSend
		var sendAt string
		if err := rows.Scan(&send.Step, &sendAt); err != nil {
			t.Fatal(err)
		}
		send.SendAt, err = parseDBTimestamp(sendAt)
		if err != nil {
			t.Fatal(err)
		}
		sends = append(sends, send)
	}
	return sends
}

// assertCohortSends checks step 1 lands on or after the cohort date (in the
// lead's timezone, within the send window) and later steps follow their delays.
func assertCohortSends(t *testing.T, label string, sends []cohortSend, cohortDate string, leadTZ *time.Location, wantDays int) {
	t.Helper()
	if len(sends) != 3 {
		t.Fatalf("%s: expected 3 sends, got %d", label, len(sends))
	}
	floor, _ := time.ParseInLocation("2006-01-02 15:04", cohortDate+" 09:00", leadTZ)
	step1 := sends[0].SendAt.In(leadTZ)
	if step1.Before(floor) {
		t.Fatalf("%s: step 1 at %s is before cohort start %s", label, step1, floor)
	}
	if step1.After(floor.AddDate(0, 0, wantDays)) {
		t.Fatalf("%s: step 1 at %s is more than %d days after cohort start %s", label, step1, wantDays, floor)
	}
	if hour := step1.Hour(); hour < 9 || hour >= 11 {
		t.Fatalf("%s: step 1 at %s is outside the 09:00-11:00 lead-local window", label, step1)
	}
	delays := []int{0, 3, 5}
	for i := 1; i < len(sends); i++ {
		prev := sends[i-1].SendAt.In(leadTZ)
		got := sends[i].SendAt.In(leadTZ)
		minDate := prev.AddDate(0, 0, delays[i]).Format("2006-01-02")
		if got.Format("2006-01-02") < minDate {
			t.Fatalf("%s: step %d at %s is earlier than its %d-day delay after %s", label, i+1, got, delays[i], prev)
		}
		if got.After(prev.AddDate(0, 0, delays[i]+3)) {
			t.Fatalf("%s: step %d at %s drifted too far after %s", label, i+1, got, prev)
		}
	}
}

func setupCohortStartCampaigns(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	db.Exec("INSERT INTO accounts (id, email, daily_limit) VALUES (1, 'sender@x.com', 20)")
	// Campaign 1 mirrors the agency draft: dated far-future cohorts.
	db.Exec(`INSERT INTO campaigns (id, name, status, sequence_file, sequence_content, start_date,
		send_window_start, send_window_end, send_days, timezone)
		VALUES (1, 'agency', 'draft', 'seq.yml', ?, '2026-10-26', '09:00', '11:00', '1,2,3,4,5', 'Europe/Oslo')`, cohortStartSeqYAML)
	// Campaign 2 mirrors the founder evergreen campaigns sharing the same inbox.
	db.Exec(`INSERT INTO campaigns (id, name, status, sequence_file, sequence_content, start_date,
		send_window_start, send_window_end, send_days, timezone)
		VALUES (2, 'founder', 'active', 'seq.yml', ?, '2026-10-01', '09:00', '11:00', '1,2,3,4,5', 'Europe/Oslo')`, cohortStartSeqYAML)
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (1, 1)")
	db.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (2, 1)")
	return db
}

func TestAddLeadsCohortStartBeyond30DaysSurvivesRebalances(t *testing.T) {
	origNow := timeNow
	timeNow = func() time.Time { return time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = origNow })

	db := setupCohortStartCampaigns(t)
	ny, _ := time.LoadLocation("America/New_York")
	oslo, _ := time.LoadLocation("Europe/Oslo")

	// D = 2026-10-26; D+90 = 2027-01-24 (a Sunday, so first send Monday 25th).
	if _, err := AddLeadsToCampaignWithOpts(db, AddLeadsToCampaignOpts{
		CampaignName: "agency",
		LeadsInline:  "email,first_name\nnear@a.com,Near\n",
		StartDate:    "2026-10-27",
	}); err != nil {
		t.Fatalf("adding near cohort: %v", err)
	}
	if _, err := AddLeadsToCampaignWithOpts(db, AddLeadsToCampaignOpts{
		CampaignName: "agency",
		LeadsInline:  "email,first_name,schedule_timezone\nfar-ny@b.com,FarNY,America/New_York\nfar-oslo@c.com,FarOslo,\n",
		StartDate:    "2027-01-24",
	}); err != nil {
		t.Fatalf("adding far cohort: %v", err)
	}

	check := func(stage string) {
		t.Helper()
		assertCohortSends(t, stage+" far-ny", loadLeadSends(t, db, 1, "far-ny@b.com"), "2027-01-24", ny, 3)
		assertCohortSends(t, stage+" far-oslo", loadLeadSends(t, db, 1, "far-oslo@c.com"), "2027-01-24", oslo, 3)
		assertCohortSends(t, stage+" near", loadLeadSends(t, db, 1, "near@a.com"), "2026-10-27", oslo, 3)
	}
	check("after add-leads")

	if err := RebalancePendingSchedules(db, []int64{1}); err != nil {
		t.Fatalf("rebalance: %v", err)
	}
	check("after rebalance")

	// A second campaign on the same inbox gets a new cohort and is rescheduled.
	if _, err := AddLeadsToCampaignWithOpts(db, AddLeadsToCampaignOpts{
		CampaignName: "founder",
		LeadsInline:  "email,first_name\nfounder@d.com,Founder\n",
		StartDate:    "2026-10-28",
	}); err != nil {
		t.Fatalf("adding founder cohort: %v", err)
	}
	check("after other campaign add-leads")

	windowEnd := "11:30"
	if err := UpdateCampaign(db, "founder", UpdateCampaignOpts{SendWindowEnd: &windowEnd}); err != nil {
		t.Fatalf("updating founder campaign: %v", err)
	}
	check("after other campaign reschedule")

	// Rescheduling the cohort's own campaign must keep its cohort floor too.
	windowStart := "09:00"
	if err := UpdateCampaign(db, "agency", UpdateCampaignOpts{SendWindowStart: &windowStart}); err != nil {
		t.Fatalf("updating agency campaign: %v", err)
	}
	check("after own campaign reschedule")
}

func TestAddLeadsPreviewOnlyKeepsCohortStartBeyond30Days(t *testing.T) {
	origNow := timeNow
	timeNow = func() time.Time { return time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = origNow })

	db := setupCohortStartCampaigns(t)

	for _, cohortDate := range []string{"2026-11-24", "2026-11-25", "2026-11-26", "2027-04-14"} {
		preview, err := AddLeadsToCampaignWithOpts(db, AddLeadsToCampaignOpts{
			CampaignName: "agency",
			LeadsInline:  "email,first_name\nlead@e.com,Lead\n",
			StartDate:    cohortDate,
			PreviewOnly:  true,
		})
		if err != nil {
			t.Fatalf("preview %s: %v", cohortDate, err)
		}
		if len(preview.Emails) != 3 {
			t.Fatalf("preview %s: expected 3 planned emails, got %d", cohortDate, len(preview.Emails))
		}
		var sends []cohortSend
		for _, email := range preview.Emails {
			at, err := parseDBTimestamp(email.SendAt)
			if err != nil {
				t.Fatal(err)
			}
			sends = append(sends, cohortSend{Step: email.StepNumber, SendAt: at})
		}
		oslo, _ := time.LoadLocation("Europe/Oslo")
		assertCohortSends(t, "preview "+cohortDate, sends, cohortDate, oslo, 3)
	}
}

func TestRebalanceStillRepairsStrandedRowsWithoutCohortStart(t *testing.T) {
	origNow := timeNow
	timeNow = func() time.Time { return time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { timeNow = origNow })

	db := setupCohortStartCampaigns(t)
	oslo, _ := time.LoadLocation("Europe/Oslo")

	// A legacy founder lead with no stored cohort start, stranded a year out.
	db.Exec("INSERT INTO leads (id, email, first_name, domain) VALUES (100, 'stranded@f.com', 'Stranded', 'f.com')")
	db.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (2, 100, 'active')")
	db.Exec(`INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, send_at, status)
		VALUES (2, 100, 1, 1, '2027-10-20T08:00:00Z', 'pending'),
		       (2, 100, 1, 2, '2027-10-25T08:00:00Z', 'pending'),
		       (2, 100, 1, 3, '2027-11-01T08:00:00Z', 'pending')`)

	if _, err := AddLeadsToCampaignWithOpts(db, AddLeadsToCampaignOpts{
		CampaignName: "agency",
		LeadsInline:  "email,first_name\nfar@g.com,Far\n",
		StartDate:    "2027-01-24",
	}); err != nil {
		t.Fatalf("adding far cohort: %v", err)
	}
	if err := RebalancePendingSchedules(db, []int64{1}); err != nil {
		t.Fatalf("rebalance: %v", err)
	}

	// The stranded legacy row is pulled back to the next send window (Thu 22 Oct).
	assertCohortSends(t, "stranded", loadLeadSends(t, db, 2, "stranded@f.com"), "2026-10-22", oslo, 1)
	assertCohortSends(t, "far", loadLeadSends(t, db, 1, "far@g.com"), "2027-01-24", oslo, 3)
}
