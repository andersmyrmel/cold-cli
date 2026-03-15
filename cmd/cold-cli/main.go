package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anders/cold-cli/internal"
	"github.com/spf13/cobra"
)

var jsonOutput bool

func openDB() (*sql.DB, error) {
	dbPath := internal.DBPath()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("cold-cli not initialized — run 'cold-cli init' first")
	}
	return internal.OpenDB(dbPath)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

var rootCmd = &cobra.Command{
	Use:   "cold-cli",
	Short: "Agent-first CLI cold email sequence engine",
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize cold-cli data directory, database, and config",
	RunE: func(cmd *cobra.Command, args []string) error {
		dataDir := internal.DataDir()

		if err := internal.EnsureDataDir(); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}

		db, err := internal.OpenDB(internal.DBPath())
		if err != nil {
			return err
		}
		db.Close()

		configPath := internal.ConfigPath()
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			if err := internal.WriteDefaultConfig(configPath); err != nil {
				return fmt.Errorf("writing config: %w", err)
			}
		}

		gwsErr := internal.CheckGWSInstalled()

		if jsonOutput {
			result := map[string]any{
				"data_dir": dataDir,
				"database": internal.DBPath(),
				"config":   configPath,
				"gws_ok":   gwsErr == nil,
			}
			if gwsErr != nil {
				result["gws_error"] = gwsErr.Error()
			}
			return printJSON(result)
		}

		fmt.Printf("Initialized cold-cli at %s\n", dataDir)
		fmt.Printf("  database: %s\n", internal.DBPath())
		fmt.Printf("  config:   %s\n", configPath)
		if gwsErr != nil {
			fmt.Printf("  warning:  %s\n", gwsErr)
		} else {
			fmt.Println("  gws:      ok")
		}
		return nil
	},
}

// --- account commands ---

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage sending accounts",
}

var accountAddCmd = &cobra.Command{
	Use:   "add <email>",
	Short: "Add a sending account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := strings.TrimSpace(args[0])
		if _, err := mail.ParseAddress(email); err != nil {
			return fmt.Errorf("invalid email address %q", email)
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		dailyLimit, _ := cmd.Flags().GetInt("daily-limit")
		skipAuth, _ := cmd.Flags().GetBool("skip-auth")

		// Set up per-account gws config directory
		configDir := internal.GWSConfigDirForAccount(email)

		// Run gws auth login for this account unless skipped
		if !skipAuth {
			fmt.Printf("Authenticating %s with gws...\n", email)
			fmt.Println("A browser window will open for Google OAuth login.")
			fmt.Println()
			if err := internal.GWSAuthLogin(configDir); err != nil {
				return fmt.Errorf("gws auth failed for %s: %w\nYou can retry with: cold-cli account add %s\nOr skip auth with: cold-cli account add %s --skip-auth", email, err, email, email)
			}
			fmt.Println()
		}

		result, err := db.Exec(
			"INSERT INTO accounts (email, daily_limit, gws_config_dir) VALUES (?, ?, ?)",
			email, dailyLimit, configDir,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("account %s already exists", email)
			}
			return fmt.Errorf("adding account: %w", err)
		}

		id, _ := result.LastInsertId()

		if jsonOutput {
			return printJSON(map[string]any{
				"id":             id,
				"email":          email,
				"daily_limit":    dailyLimit,
				"status":         "active",
				"gws_config_dir": configDir,
			})
		}

		fmt.Printf("Added account %s (id=%d, daily_limit=%d)\n", email, id, dailyLimit)
		return nil
	},
}

var accountListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sending accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		rows, err := db.Query("SELECT id, email, daily_limit, status FROM accounts ORDER BY id")
		if err != nil {
			return fmt.Errorf("querying accounts: %w", err)
		}
		defer rows.Close()

		type accountRow struct {
			ID         int64  `json:"id"`
			Email      string `json:"email"`
			DailyLimit int    `json:"daily_limit"`
			Status     string `json:"status"`
		}

		var accounts []accountRow
		for rows.Next() {
			var a accountRow
			if err := rows.Scan(&a.ID, &a.Email, &a.DailyLimit, &a.Status); err != nil {
				return fmt.Errorf("scanning account: %w", err)
			}
			accounts = append(accounts, a)
		}

		if jsonOutput {
			return printJSON(accounts)
		}

		if len(accounts) == 0 {
			fmt.Println("No accounts. Add one with: cold-cli account add <email>")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tEMAIL\tDAILY LIMIT\tSTATUS")
		for _, a := range accounts {
			fmt.Fprintf(w, "%d\t%s\t%d\t%s\n", a.ID, a.Email, a.DailyLimit, a.Status)
		}
		return w.Flush()
	},
}

// --- lead commands ---

var leadCmd = &cobra.Command{
	Use:   "lead",
	Short: "Manage leads",
}

var leadPauseCmd = &cobra.Command{
	Use:   "pause <email>",
	Short: "Pause a lead across all campaigns",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := strings.TrimSpace(args[0])

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		// Find lead
		var leadID int64
		err = db.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("lead %s not found", email)
		}
		if err != nil {
			return fmt.Errorf("looking up lead: %w", err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("starting transaction: %w", err)
		}
		defer tx.Rollback()

		// Pause in all campaigns
		res, err := tx.Exec(
			"UPDATE campaign_leads SET status = 'paused' WHERE lead_id = ? AND status IN ('active', 'pending')",
			leadID,
		)
		if err != nil {
			return fmt.Errorf("pausing campaign_leads: %w", err)
		}
		pausedCampaigns, _ := res.RowsAffected()

		// Cancel pending sends
		res, err = tx.Exec(
			"UPDATE scheduled_sends SET status = 'cancelled' WHERE lead_id = ? AND status = 'pending'",
			leadID,
		)
		if err != nil {
			return fmt.Errorf("cancelling sends: %w", err)
		}
		cancelledSends, _ := res.RowsAffected()

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing: %w", err)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"email":             email,
				"paused_campaigns":  pausedCampaigns,
				"cancelled_sends":   cancelledSends,
			})
		}

		fmt.Printf("Paused %s: %d campaigns paused, %d sends cancelled\n", email, pausedCampaigns, cancelledSends)
		return nil
	},
}

var leadBlacklistCmd = &cobra.Command{
	Use:   "blacklist <email|domain>",
	Short: "Blacklist a lead by email or all leads on a domain",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := strings.TrimSpace(args[0])

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("starting transaction: %w", err)
		}
		defer tx.Rollback()

		// Determine if this is an email or domain
		isDomain := !strings.Contains(target, "@")

		var blacklistedLeads int64
		var cancelledSends int64

		if isDomain {
			domain := strings.ToLower(target)

			// Blacklist all leads on this domain
			res, err := tx.Exec(
				"UPDATE leads SET global_status = 'blacklisted' WHERE domain = ? AND global_status != 'blacklisted'",
				domain,
			)
			if err != nil {
				return fmt.Errorf("blacklisting domain: %w", err)
			}
			blacklistedLeads, _ = res.RowsAffected()

			// Cancel pending sends for all leads on this domain
			res, err = tx.Exec(`
				UPDATE scheduled_sends SET status = 'cancelled'
				WHERE lead_id IN (SELECT id FROM leads WHERE domain = ?)
				AND status = 'pending'`,
				domain,
			)
			if err != nil {
				return fmt.Errorf("cancelling sends: %w", err)
			}
			cancelledSends, _ = res.RowsAffected()

			// Update campaign_leads status
			tx.Exec(`
				UPDATE campaign_leads SET status = 'paused'
				WHERE lead_id IN (SELECT id FROM leads WHERE domain = ?)
				AND status IN ('active', 'pending')`,
				domain,
			)
		} else {
			// Blacklist single email
			res, err := tx.Exec(
				"UPDATE leads SET global_status = 'blacklisted' WHERE email = ? AND global_status != 'blacklisted'",
				target,
			)
			if err != nil {
				return fmt.Errorf("blacklisting lead: %w", err)
			}
			blacklistedLeads, _ = res.RowsAffected()
			if blacklistedLeads == 0 {
				return fmt.Errorf("lead %s not found or already blacklisted", target)
			}

			// Cancel pending sends
			res, err = tx.Exec(`
				UPDATE scheduled_sends SET status = 'cancelled'
				WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
				AND status = 'pending'`,
				target,
			)
			if err != nil {
				return fmt.Errorf("cancelling sends: %w", err)
			}
			cancelledSends, _ = res.RowsAffected()

			// Update campaign_leads status
			tx.Exec(`
				UPDATE campaign_leads SET status = 'paused'
				WHERE lead_id = (SELECT id FROM leads WHERE email = ?)
				AND status IN ('active', 'pending')`,
				target,
			)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing: %w", err)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"target":            target,
				"is_domain":         isDomain,
				"blacklisted_leads": blacklistedLeads,
				"cancelled_sends":   cancelledSends,
			})
		}

		if isDomain {
			fmt.Printf("Blacklisted domain %s: %d leads blacklisted, %d sends cancelled\n", target, blacklistedLeads, cancelledSends)
		} else {
			fmt.Printf("Blacklisted %s: %d sends cancelled\n", target, cancelledSends)
		}
		return nil
	},
}

// --- campaign commands ---

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Manage campaigns",
}

var campaignCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new campaign from a sequence YAML and leads CSV",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		seqFile, _ := cmd.Flags().GetString("sequence")
		leadsFile, _ := cmd.Flags().GetString("leads")
		accountsFlag, _ := cmd.Flags().GetString("accounts")

		if name == "" || seqFile == "" || leadsFile == "" || accountsFlag == "" {
			return fmt.Errorf("all flags required: --name, --sequence, --leads, --accounts")
		}

		// Parse sequence YAML
		seq, err := internal.ParseSequence(seqFile)
		if err != nil {
			return err
		}

		// Parse leads CSV
		records, _, err := internal.ParseLeadsCSV(leadsFile)
		if err != nil {
			return err
		}

		// Validate template fields
		placeholders := seq.CollectPlaceholders()
		if err := internal.ValidateLeadFields(records, placeholders); err != nil {
			return err
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		// Resolve account emails to IDs
		accountEmails := strings.Split(accountsFlag, ",")
		var accountIDs []int64
		for _, email := range accountEmails {
			email = strings.TrimSpace(email)
			var id int64
			err := db.QueryRow("SELECT id FROM accounts WHERE email = ? AND status = 'active'", email).Scan(&id)
			if err == sql.ErrNoRows {
				return fmt.Errorf("account %s not found or not active", email)
			}
			if err != nil {
				return fmt.Errorf("looking up account %s: %w", email, err)
			}
			accountIDs = append(accountIDs, id)
		}

		// Load config for defaults
		cfg, err := internal.LoadConfig()
		if err != nil {
			return err
		}

		sendDays, err := internal.ParseSendDays(cfg.SendDays)
		if err != nil {
			return fmt.Errorf("parsing send_days: %w", err)
		}

		tz, err := time.LoadLocation(cfg.DefaultTimezone)
		if err != nil {
			return fmt.Errorf("loading timezone %s: %w", cfg.DefaultTimezone, err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("starting transaction: %w", err)
		}
		defer tx.Rollback()

		// Insert campaign
		result, err := tx.Exec(`
			INSERT INTO campaigns (name, status, sequence_file, send_window_start, send_window_end,
				send_days, timezone, min_gap_seconds, max_gap_seconds)
			VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?)`,
			name, seqFile,
			cfg.SendWindowStart, cfg.SendWindowEnd,
			cfg.SendDays, cfg.DefaultTimezone,
			cfg.MinGapSeconds, cfg.MaxGapSeconds,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("campaign %q already exists", name)
			}
			return fmt.Errorf("inserting campaign: %w", err)
		}
		campaignID, _ := result.LastInsertId()

		// Insert campaign_accounts
		for _, accID := range accountIDs {
			if _, err := tx.Exec("INSERT INTO campaign_accounts (campaign_id, account_id) VALUES (?, ?)", campaignID, accID); err != nil {
				return fmt.Errorf("linking account: %w", err)
			}
		}

		// Insert leads (upsert — lead may already exist from another campaign)
		var leadsForSchedule []internal.LeadForSchedule
		for _, rec := range records {
			email := rec.Fields["email"]
			domain := internal.ExtractDomain(email)
			firstName := rec.Fields["first_name"]
			lastName := rec.Fields["last_name"]
			company := rec.Fields["company"]

			// Try insert, ignore if exists
			tx.Exec(`INSERT OR IGNORE INTO leads (email, first_name, last_name, company, domain)
				VALUES (?, ?, ?, ?, ?)`,
				email, firstName, lastName, company, domain,
			)

			var leadID int64
			if err := tx.QueryRow("SELECT id FROM leads WHERE email = ?", email).Scan(&leadID); err != nil {
				return fmt.Errorf("looking up lead %s: %w", email, err)
			}

			// Check if lead is globally blacklisted/bounced
			var globalStatus string
			tx.QueryRow("SELECT global_status FROM leads WHERE id = ?", leadID).Scan(&globalStatus)
			if globalStatus == "blacklisted" || globalStatus == "bounced" {
				continue // skip this lead
			}

			// Insert campaign_lead
			if _, err := tx.Exec("INSERT INTO campaign_leads (campaign_id, lead_id, status) VALUES (?, ?, 'active')",
				campaignID, leadID); err != nil {
				return fmt.Errorf("linking lead %s: %w", email, err)
			}

			leadsForSchedule = append(leadsForSchedule, internal.LeadForSchedule{
				ID:     leadID,
				Fields: rec.Fields,
			})
		}

		if len(leadsForSchedule) == 0 {
			return fmt.Errorf("no eligible leads (all blacklisted or bounced)")
		}

		// Compute schedule
		schedRows, err := internal.ComputeSchedule(internal.ScheduleConfig{
			CampaignID:      campaignID,
			AccountIDs:      accountIDs,
			Leads:           leadsForSchedule,
			Sequence:        seq,
			SendWindowStart: cfg.SendWindowStart,
			SendWindowEnd:   cfg.SendWindowEnd,
			SendDays:        sendDays,
			Timezone:        tz,
			MinGapSeconds:   cfg.MinGapSeconds,
			MaxGapSeconds:   cfg.MaxGapSeconds,
			StartTime:       time.Now().In(tz),
		})
		if err != nil {
			return fmt.Errorf("computing schedule: %w", err)
		}

		// Insert scheduled_sends
		for _, row := range schedRows {
			if _, err := tx.Exec(`
				INSERT INTO scheduled_sends (campaign_id, lead_id, account_id, step_number, variant_index, send_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				row.CampaignID, row.LeadID, row.AccountID,
				row.StepNumber, row.VariantIndex, row.SendAt.UTC().Format(time.RFC3339),
			); err != nil {
				return fmt.Errorf("inserting scheduled_send: %w", err)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing: %w", err)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"id":              campaignID,
				"name":            name,
				"status":          "draft",
				"leads":           len(leadsForSchedule),
				"scheduled_sends": len(schedRows),
				"accounts":        len(accountIDs),
			})
		}

		fmt.Printf("Created campaign %q (id=%d)\n", name, campaignID)
		fmt.Printf("  leads:    %d\n", len(leadsForSchedule))
		fmt.Printf("  sends:    %d\n", len(schedRows))
		fmt.Printf("  accounts: %d\n", len(accountIDs))
		fmt.Printf("  status:   draft\n")
		fmt.Printf("\nRun 'cold-cli campaign preview %s' to review the schedule.\n", name)
		return nil
	},
}

var campaignPreviewCmd = &cobra.Command{
	Use:   "preview <name>",
	Short: "Preview the full send schedule for a campaign",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		var campaignID int64
		var status string
		err = db.QueryRow("SELECT id, status FROM campaigns WHERE name = ?", name).Scan(&campaignID, &status)
		if err == sql.ErrNoRows {
			return fmt.Errorf("campaign %q not found", name)
		}
		if err != nil {
			return fmt.Errorf("looking up campaign: %w", err)
		}

		rows, err := db.Query(`
			SELECT ss.step_number, ss.variant_index, ss.send_at, ss.status,
				l.email, a.email
			FROM scheduled_sends ss
			JOIN leads l ON ss.lead_id = l.id
			JOIN accounts a ON ss.account_id = a.id
			WHERE ss.campaign_id = ?
			ORDER BY ss.send_at, ss.step_number`,
			campaignID,
		)
		if err != nil {
			return fmt.Errorf("querying schedule: %w", err)
		}
		defer rows.Close()

		type previewRow struct {
			StepNumber   int    `json:"step_number"`
			VariantIndex int    `json:"variant_index"`
			SendAt       string `json:"send_at"`
			Status       string `json:"status"`
			LeadEmail    string `json:"lead_email"`
			AccountEmail string `json:"account_email"`
		}

		var preview []previewRow
		for rows.Next() {
			var r previewRow
			if err := rows.Scan(&r.StepNumber, &r.VariantIndex, &r.SendAt, &r.Status, &r.LeadEmail, &r.AccountEmail); err != nil {
				return fmt.Errorf("scanning row: %w", err)
			}
			preview = append(preview, r)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"campaign": name,
				"status":   status,
				"total":    len(preview),
				"schedule": preview,
			})
		}

		if len(preview) == 0 {
			fmt.Printf("Campaign %q has no scheduled sends.\n", name)
			return nil
		}

		fmt.Printf("Campaign: %s (status: %s, %d sends)\n\n", name, status, len(preview))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SEND AT\tSTEP\tVARIANT\tLEAD\tACCOUNT\tSTATUS")
		for _, r := range preview {
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n",
				r.SendAt, r.StepNumber, r.VariantIndex, r.LeadEmail, r.AccountEmail, r.Status)
		}
		return w.Flush()
	},
}

var campaignActivateCmd = &cobra.Command{
	Use:   "activate <name>",
	Short: "Activate a draft campaign so tick will process it",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateTransition("activate", "draft", "active"),
}

var campaignPauseCmd = &cobra.Command{
	Use:   "pause <name>",
	Short: "Pause an active campaign",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateTransition("pause", "active", "paused"),
}

var campaignResumeCmd = &cobra.Command{
	Use:   "resume <name>",
	Short: "Resume a paused campaign",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateTransition("resume", "paused", "active"),
}

func campaignStateTransition(action, fromStatus, toStatus string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		name := args[0]

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		var currentStatus string
		err = db.QueryRow("SELECT status FROM campaigns WHERE name = ?", name).Scan(&currentStatus)
		if err == sql.ErrNoRows {
			return fmt.Errorf("campaign %q not found", name)
		}
		if err != nil {
			return fmt.Errorf("looking up campaign: %w", err)
		}

		if currentStatus != fromStatus {
			return fmt.Errorf("cannot %s campaign %q: current status is %q (expected %q)", action, name, currentStatus, fromStatus)
		}

		if _, err := db.Exec("UPDATE campaigns SET status = ? WHERE name = ?", toStatus, name); err != nil {
			return fmt.Errorf("updating campaign: %w", err)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"name":   name,
				"status": toStatus,
			})
		}

		fmt.Printf("Campaign %q is now %s.\n", name, toStatus)
		return nil
	}
}

var campaignStatusCmd = &cobra.Command{
	Use:   "status <name>",
	Short: "Show campaign details and send counts by status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		var c struct {
			ID     int64
			Status string
			SeqFile string
			Timezone string
			WindowStart string
			WindowEnd string
			CreatedAt string
		}
		err = db.QueryRow(`SELECT id, status, sequence_file, timezone, send_window_start, send_window_end, created_at
			FROM campaigns WHERE name = ?`, name).
			Scan(&c.ID, &c.Status, &c.SeqFile, &c.Timezone, &c.WindowStart, &c.WindowEnd, &c.CreatedAt)
		if err == sql.ErrNoRows {
			return fmt.Errorf("campaign %q not found", name)
		}
		if err != nil {
			return fmt.Errorf("looking up campaign: %w", err)
		}

		// Count sends by status
		countRows, err := db.Query(`
			SELECT status, COUNT(*) FROM scheduled_sends
			WHERE campaign_id = ? GROUP BY status`, c.ID)
		if err != nil {
			return fmt.Errorf("counting sends: %w", err)
		}
		defer countRows.Close()

		counts := map[string]int{}
		total := 0
		for countRows.Next() {
			var status string
			var count int
			countRows.Scan(&status, &count)
			counts[status] = count
			total += count
		}

		// Count leads
		var leadCount int
		db.QueryRow("SELECT COUNT(*) FROM campaign_leads WHERE campaign_id = ?", c.ID).Scan(&leadCount)

		// Count accounts
		var accountCount int
		db.QueryRow("SELECT COUNT(*) FROM campaign_accounts WHERE campaign_id = ?", c.ID).Scan(&accountCount)

		if jsonOutput {
			return printJSON(map[string]any{
				"name":         name,
				"status":       c.Status,
				"sequence":     c.SeqFile,
				"timezone":     c.Timezone,
				"send_window":  c.WindowStart + " - " + c.WindowEnd,
				"leads":        leadCount,
				"accounts":     accountCount,
				"total_sends":  total,
				"send_counts":  counts,
				"created_at":   c.CreatedAt,
			})
		}

		fmt.Printf("Campaign: %s\n", name)
		fmt.Printf("  status:      %s\n", c.Status)
		fmt.Printf("  sequence:    %s\n", c.SeqFile)
		fmt.Printf("  timezone:    %s\n", c.Timezone)
		fmt.Printf("  send window: %s - %s\n", c.WindowStart, c.WindowEnd)
		fmt.Printf("  leads:       %d\n", leadCount)
		fmt.Printf("  accounts:    %d\n", accountCount)
		fmt.Printf("  created:     %s\n", c.CreatedAt)
		fmt.Printf("\nScheduled sends: %d total\n", total)
		for _, s := range []string{"pending", "sent", "failed", "skipped", "cancelled"} {
			if n, ok := counts[s]; ok {
				fmt.Printf("  %-10s %d\n", s, n)
			}
		}
		return nil
	},
}

// --- tick command ---

var tickCmd = &cobra.Command{
	Use:   "tick",
	Short: "Run one tick cycle: poll replies/bounces, send due emails",
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		// Acquire lock (skip for dry-run)
		if !dryRun {
			if err := internal.EnsureDataDir(); err != nil {
				return err
			}
			lockFile, err := internal.AcquireTickLock()
			if err != nil {
				if jsonOutput {
					return printJSON(map[string]any{"status": "locked", "message": "tick already running"})
				}
				fmt.Println("tick already running")
				return nil
			}
			defer lockFile.Close()
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		// Load per-account gws config dirs
		gwsCLI := internal.NewGWSCLI()
		rows, err := db.Query("SELECT email, gws_config_dir FROM accounts WHERE status = 'active' AND gws_config_dir != ''")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var email, configDir string
				rows.Scan(&email, &configDir)
				gwsCLI.SetConfigDir(email, configDir)
			}
		}

		result, err := internal.Tick(internal.TickConfig{
			DB:     db,
			GWS:    gwsCLI,
			DryRun: dryRun,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Println(internal.FormatTickResult(result))
		return nil
	},
}

// --- stats command ---

var statsCmd = &cobra.Command{
	Use:   "stats [campaign]",
	Short: "Show send/reply/bounce statistics",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		perLeads, _ := cmd.Flags().GetBool("leads")

		// If a specific campaign is given
		if len(args) == 1 {
			name := args[0]

			var campaignID int64
			err := db.QueryRow("SELECT id FROM campaigns WHERE name = ?", name).Scan(&campaignID)
			if err == sql.ErrNoRows {
				return fmt.Errorf("campaign %q not found", name)
			}
			if err != nil {
				return fmt.Errorf("looking up campaign: %w", err)
			}

			if perLeads {
				return showLeadStats(db, campaignID, name)
			}
			return showCampaignStepStats(db, campaignID, name)
		}

		// All campaigns summary
		return showAllCampaignStats(db)
	},
}

func showAllCampaignStats(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT c.name, c.status,
			COALESCE(SUM(CASE WHEN e.type = 'sent' THEN 1 ELSE 0 END), 0) as sent,
			COALESCE(SUM(CASE WHEN e.type = 'reply' THEN 1 ELSE 0 END), 0) as replies,
			COALESCE(SUM(CASE WHEN e.type = 'bounce' THEN 1 ELSE 0 END), 0) as bounces
		FROM campaigns c
		LEFT JOIN events e ON c.id = e.campaign_id
		GROUP BY c.id, c.name, c.status
		ORDER BY c.created_at DESC`)
	if err != nil {
		return fmt.Errorf("querying stats: %w", err)
	}
	defer rows.Close()

	type campaignStats struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Sent    int    `json:"sent"`
		Replies int    `json:"replies"`
		Bounces int    `json:"bounces"`
	}

	var stats []campaignStats
	for rows.Next() {
		var s campaignStats
		rows.Scan(&s.Name, &s.Status, &s.Sent, &s.Replies, &s.Bounces)
		stats = append(stats, s)
	}

	if jsonOutput {
		return printJSON(stats)
	}

	if len(stats) == 0 {
		fmt.Println("No campaigns. Create one with: cold-cli campaign create")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "CAMPAIGN\tSTATUS\tSENT\tREPLIES\tBOUNCES")
	for _, s := range stats {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\n", s.Name, s.Status, s.Sent, s.Replies, s.Bounces)
	}
	return w.Flush()
}

func showCampaignStepStats(db *sql.DB, campaignID int64, name string) error {
	rows, err := db.Query(`
		SELECT e.step_number,
			SUM(CASE WHEN e.type = 'sent' THEN 1 ELSE 0 END) as sent,
			SUM(CASE WHEN e.type = 'reply' THEN 1 ELSE 0 END) as replies,
			SUM(CASE WHEN e.type = 'bounce' THEN 1 ELSE 0 END) as bounces
		FROM events e
		WHERE e.campaign_id = ?
		GROUP BY e.step_number
		ORDER BY e.step_number`, campaignID)
	if err != nil {
		return fmt.Errorf("querying step stats: %w", err)
	}
	defer rows.Close()

	type stepStats struct {
		Step    int `json:"step"`
		Sent    int `json:"sent"`
		Replies int `json:"replies"`
		Bounces int `json:"bounces"`
	}

	var stats []stepStats
	for rows.Next() {
		var s stepStats
		rows.Scan(&s.Step, &s.Sent, &s.Replies, &s.Bounces)
		stats = append(stats, s)
	}

	if jsonOutput {
		return printJSON(map[string]any{
			"campaign": name,
			"steps":    stats,
		})
	}

	if len(stats) == 0 {
		fmt.Printf("Campaign %q has no events yet.\n", name)
		return nil
	}

	fmt.Printf("Campaign: %s\n\n", name)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STEP\tSENT\tREPLIES\tBOUNCES")
	for _, s := range stats {
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\n", s.Step, s.Sent, s.Replies, s.Bounces)
	}
	return w.Flush()
}

func showLeadStats(db *sql.DB, campaignID int64, name string) error {
	rows, err := db.Query(`
		SELECT l.email, cl.status,
			(SELECT COUNT(*) FROM events e WHERE e.lead_id = l.id AND e.campaign_id = ? AND e.type = 'sent') as steps_sent,
			(SELECT MAX(e.timestamp) FROM events e WHERE e.lead_id = l.id AND e.campaign_id = ? AND e.type = 'reply') as reply_at
		FROM campaign_leads cl
		JOIN leads l ON cl.lead_id = l.id
		WHERE cl.campaign_id = ?
		ORDER BY l.email`, campaignID, campaignID, campaignID)
	if err != nil {
		return fmt.Errorf("querying lead stats: %w", err)
	}
	defer rows.Close()

	type leadStats struct {
		Email     string  `json:"email"`
		Status    string  `json:"status"`
		StepsSent int     `json:"steps_sent"`
		ReplyAt   *string `json:"reply_at,omitempty"`
	}

	var stats []leadStats
	for rows.Next() {
		var s leadStats
		var replyAt sql.NullString
		rows.Scan(&s.Email, &s.Status, &s.StepsSent, &replyAt)
		if replyAt.Valid {
			s.ReplyAt = &replyAt.String
		}
		stats = append(stats, s)
	}

	if jsonOutput {
		return printJSON(map[string]any{
			"campaign": name,
			"leads":    stats,
		})
	}

	if len(stats) == 0 {
		fmt.Printf("Campaign %q has no leads.\n", name)
		return nil
	}

	fmt.Printf("Campaign: %s\n\n", name)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tSTATUS\tSTEPS SENT\tREPLY AT")
	for _, s := range stats {
		replyAt := "-"
		if s.ReplyAt != nil {
			replyAt = *s.ReplyAt
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.Email, s.Status, s.StepsSent, replyAt)
	}
	return w.Flush()
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	accountAddCmd.Flags().Int("daily-limit", 50, "maximum emails per day for this account")
	accountAddCmd.Flags().Bool("skip-auth", false, "skip gws OAuth login (for testing or pre-authed accounts)")
	accountCmd.AddCommand(accountAddCmd, accountListCmd)
	leadCmd.AddCommand(leadPauseCmd, leadBlacklistCmd)

	campaignCreateCmd.Flags().String("name", "", "campaign name")
	campaignCreateCmd.Flags().String("sequence", "", "path to sequence YAML file")
	campaignCreateCmd.Flags().String("leads", "", "path to leads CSV file")
	campaignCreateCmd.Flags().String("accounts", "", "comma-separated account emails")
	campaignCmd.AddCommand(campaignCreateCmd, campaignPreviewCmd, campaignActivateCmd, campaignPauseCmd, campaignResumeCmd, campaignStatusCmd)

	tickCmd.Flags().Bool("dry-run", false, "show what would be sent without actually sending")

	statsCmd.Flags().Bool("leads", false, "show per-lead breakdown")

	rootCmd.AddCommand(initCmd, accountCmd, leadCmd, campaignCmd, tickCmd, statsCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
