package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"path/filepath"
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

		dailyLimit, _ := cmd.Flags().GetInt("daily-limit")
		skipAuth, _ := cmd.Flags().GetBool("skip-auth")

		configDir := internal.GWSConfigDirForAccount(email)

		if !skipAuth {
			fmt.Printf("Authenticating %s with gws...\n", email)
			fmt.Println("A browser window will open for Google OAuth login.")
			fmt.Println()
			if err := internal.GWSAuthLogin(configDir); err != nil {
				return fmt.Errorf("gws auth failed for %s: %w\nYou can retry with: cold-cli account add %s\nOr skip auth with: cold-cli account add %s --skip-auth", email, err, email, email)
			}
			fmt.Println()
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.AddAccount(db, email, dailyLimit, configDir)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Added account %s (id=%d, daily_limit=%d)\n", result.Email, result.ID, result.DailyLimit)
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

		accounts, err := internal.ListAccounts(db)
		if err != nil {
			return err
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

var accountPauseCmd = &cobra.Command{
	Use:   "pause <email>",
	Short: "Pause an account (stops sending, cancels pending sends)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.PauseAccount(db, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}
		fmt.Printf("Paused account %s: %d sends cancelled\n", result.Email, result.CancelledSends)
		return nil
	},
}

var accountResumeCmd = &cobra.Command{
	Use:   "resume <email>",
	Short: "Resume a paused account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		email := strings.TrimSpace(args[0])
		if err := internal.ResumeAccount(db, email); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"email": email, "status": "active"})
		}
		fmt.Printf("Resumed account %s\n", email)
		return nil
	},
}

var accountRemoveCmd = &cobra.Command{
	Use:   "remove <email>",
	Short: "Remove an account and cancel its pending sends",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.RemoveAccount(db, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}
		fmt.Printf("Removed account %s: %d sends cancelled\n", result.Email, result.CancelledSends)
		return nil
	},
}

// --- lead commands ---

var leadCmd = &cobra.Command{
	Use:   "lead",
	Short: "Manage leads",
}

var leadListCmd = &cobra.Command{
	Use:   "list",
	Short: "List leads, optionally filtered by domain or status",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		domain, _ := cmd.Flags().GetString("domain")
		status, _ := cmd.Flags().GetString("status")
		limit, _ := cmd.Flags().GetInt("limit")

		leads, err := internal.ListLeads(db, domain, status, limit)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(leads)
		}

		if len(leads) == 0 {
			fmt.Println("No leads found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tEMAIL\tNAME\tCOMPANY\tDOMAIN\tSTATUS\tCAMPAIGNS")
		for _, l := range leads {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\n",
				l.ID, l.Email, l.FirstName, l.Company, l.Domain, l.GlobalStatus, l.Campaigns)
		}
		return w.Flush()
	},
}

var leadPauseCmd = &cobra.Command{
	Use:   "pause <email>",
	Short: "Pause a lead across all campaigns",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.PauseLead(db, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Paused %s: %d campaigns paused, %d sends cancelled\n",
			result.Email, result.PausedCampaigns, result.CancelledSends)
		return nil
	},
}

var leadBlacklistCmd = &cobra.Command{
	Use:   "blacklist <email|domain>",
	Short: "Blacklist a lead by email or all leads on a domain",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.BlacklistLead(db, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		if result.IsDomain {
			fmt.Printf("Blacklisted domain %s: %d leads blacklisted, %d sends cancelled\n",
				result.Target, result.BlacklistedLeads, result.CancelledSends)
		} else {
			fmt.Printf("Blacklisted %s: %d sends cancelled\n", result.Target, result.CancelledSends)
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

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.CreateCampaign(db, internal.CreateCampaignOpts{
			Name:          name,
			SequenceFile:  seqFile,
			LeadsFile:     leadsFile,
			AccountEmails: strings.Split(accountsFlag, ","),
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Created campaign %q (id=%d)\n", result.Name, result.ID)
		fmt.Printf("  leads:    %d\n", result.Leads)
		fmt.Printf("  sends:    %d\n", result.ScheduledSends)
		fmt.Printf("  accounts: %d\n", result.Accounts)
		fmt.Printf("  status:   %s\n", result.Status)
		fmt.Printf("\nRun 'cold-cli campaign preview %s' to review the schedule.\n", result.Name)
		return nil
	},
}

var campaignPreviewCmd = &cobra.Command{
	Use:   "preview <name>",
	Short: "Preview the full send schedule for a campaign",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		_, status, preview, err := internal.GetCampaignPreview(db, args[0])
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"campaign": args[0],
				"status":   status,
				"total":    len(preview),
				"schedule": preview,
			})
		}

		if len(preview) == 0 {
			fmt.Printf("Campaign %q has no scheduled sends.\n", args[0])
			return nil
		}

		fmt.Printf("Campaign: %s (status: %s, %d sends)\n", args[0], status, len(preview))
		fmt.Println("Note: daily limits and send windows are enforced at send time, not shown here.")
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SEND AT\tSTEP\tVARIANT\tLEAD\tACCOUNT\tSTATUS")
		for _, r := range preview {
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n",
				r.SendAt, r.StepNumber, r.VariantIndex, r.LeadEmail, r.AccountEmail, r.Status)
		}
		return w.Flush()
	},
}

var campaignListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all campaigns",
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		campaigns, err := internal.ListCampaigns(db)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(campaigns)
		}

		if len(campaigns) == 0 {
			fmt.Println("No campaigns. Create one with: cold-cli campaign create")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tLEADS\tSENDS")
		for _, c := range campaigns {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\n", c.ID, c.Name, c.Status, c.Leads, c.Sends)
		}
		return w.Flush()
	},
}

var campaignDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a campaign and all associated data",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		id, err := internal.DeleteCampaign(db, args[0])
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": args[0], "id": id, "deleted": true})
		}

		fmt.Printf("Deleted campaign %q (id=%d)\n", args[0], id)
		return nil
	},
}

var campaignUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update campaign settings (send window, days, gaps)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		opts := internal.UpdateCampaignOpts{}
		changed := false

		if cmd.Flags().Changed("send-window-start") {
			v, _ := cmd.Flags().GetString("send-window-start")
			opts.SendWindowStart = &v
			changed = true
		}
		if cmd.Flags().Changed("send-window-end") {
			v, _ := cmd.Flags().GetString("send-window-end")
			opts.SendWindowEnd = &v
			changed = true
		}
		if cmd.Flags().Changed("send-days") {
			v, _ := cmd.Flags().GetString("send-days")
			opts.SendDays = &v
			changed = true
		}
		if cmd.Flags().Changed("timezone") {
			v, _ := cmd.Flags().GetString("timezone")
			opts.Timezone = &v
			changed = true
		}
		if cmd.Flags().Changed("min-gap") {
			v, _ := cmd.Flags().GetInt("min-gap")
			opts.MinGapSeconds = &v
			changed = true
		}
		if cmd.Flags().Changed("max-gap") {
			v, _ := cmd.Flags().GetInt("max-gap")
			opts.MaxGapSeconds = &v
			changed = true
		}

		if !changed {
			return fmt.Errorf("no settings to update — use flags like --send-days, --send-window-start, etc.")
		}

		if err := internal.UpdateCampaign(db, args[0], opts); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": args[0], "updated": true})
		}

		fmt.Printf("Updated campaign %q\n", args[0])
		return nil
	},
}

var campaignCloneCmd = &cobra.Command{
	Use:   "clone <source-name>",
	Short: "Clone a campaign with new leads (copies sequence + settings)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		leadsFile, _ := cmd.Flags().GetString("leads")
		accountsFlag, _ := cmd.Flags().GetString("accounts")

		if name == "" || leadsFile == "" {
			return fmt.Errorf("required flags: --name, --leads")
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		var accounts []string
		if accountsFlag != "" {
			accounts = strings.Split(accountsFlag, ",")
		}

		result, err := internal.CloneCampaign(db, internal.CloneCampaignOpts{
			SourceName: args[0],
			NewName:    name,
			LeadsFile:  leadsFile,
			Accounts:   accounts,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Cloned %q → %q (id=%d)\n", args[0], result.Name, result.ID)
		fmt.Printf("  leads:    %d\n", result.Leads)
		fmt.Printf("  sends:    %d\n", result.ScheduledSends)
		fmt.Printf("  accounts: %d\n", result.Accounts)
		fmt.Printf("  status:   %s\n", result.Status)
		fmt.Printf("\nRun 'cold-cli campaign preview %s' to review the schedule.\n", result.Name)
		return nil
	},
}

var campaignAddLeadsCmd = &cobra.Command{
	Use:   "add-leads <name>",
	Short: "Add new leads to an existing campaign",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		leadsFile, _ := cmd.Flags().GetString("leads")
		if leadsFile == "" {
			return fmt.Errorf("required flag: --leads")
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.AddLeadsToCampaign(db, args[0], leadsFile)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Added leads to %q\n", result.Campaign)
		fmt.Printf("  added:   %d\n", result.LeadsAdded)
		fmt.Printf("  skipped: %d (already in campaign, blacklisted, or bounced)\n", result.LeadsSkipped)
		fmt.Printf("  sends:   %d scheduled\n", result.ScheduledSends)
		return nil
	},
}

var campaignActivateCmd = &cobra.Command{
	Use:   "activate <name>",
	Short: "Activate a draft campaign so tick will process it",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateCmd("activate", "draft", "active"),
}

var campaignPauseCmd = &cobra.Command{
	Use:   "pause <name>",
	Short: "Pause an active campaign",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateCmd("pause", "active", "paused"),
}

var campaignResumeCmd = &cobra.Command{
	Use:   "resume <name>",
	Short: "Resume a paused campaign",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateCmd("resume", "paused", "active"),
}

func campaignStateCmd(action, from, to string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		if err := internal.CampaignStateTransition(db, args[0], action, from, to); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": args[0], "status": to})
		}

		fmt.Printf("Campaign %q is now %s.\n", args[0], to)
		return nil
	}
}

var campaignStatusCmd = &cobra.Command{
	Use:   "status <name>",
	Short: "Show campaign details and send counts by status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		info, err := internal.GetCampaignStatus(db, args[0])
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(info)
		}

		fmt.Printf("Campaign: %s\n", info.Name)
		fmt.Printf("  status:      %s\n", info.Status)
		fmt.Printf("  sequence:    %s\n", info.Sequence)
		fmt.Printf("  timezone:    %s\n", info.Timezone)
		fmt.Printf("  send window: %s\n", info.SendWindow)
		fmt.Printf("  leads:       %d\n", info.Leads)
		fmt.Printf("  accounts:    %d\n", info.Accounts)
		fmt.Printf("  created:     %s\n", info.CreatedAt)
		if info.ReplyRate != nil {
			fmt.Printf("  reply rate:  %.1f%%\n", *info.ReplyRate)
		}
		if info.LastSendAt != nil {
			fmt.Printf("  last send:   %s\n", *info.LastSendAt)
		}
		if info.NextSendAt != nil {
			fmt.Printf("  next send:   %s\n", *info.NextSendAt)
		}
		fmt.Printf("\nScheduled sends: %d total\n", info.TotalSends)
		for _, s := range []string{"pending", "sent", "failed", "skipped", "cancelled"} {
			if n, ok := info.SendCounts[s]; ok {
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

		// Set up structured JSON logging to ~/.cold-cli/tick.log
		logPath := filepath.Join(internal.DataDir(), "tick.log")
		if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			defer logFile.Close()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logFile, nil)))
		}

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

		// Load timezone for daily limit calculation
		cfg, _ := internal.LoadConfig()
		var tz *time.Location
		if cfg != nil {
			tz, _ = time.LoadLocation(cfg.DefaultTimezone)
		}

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

		var unsubHeader bool
		unsubSubject := "Unsubscribe"
		if cfg != nil {
			unsubHeader = cfg.UnsubscribeHeader
			if cfg.UnsubscribeSubject != "" {
				unsubSubject = cfg.UnsubscribeSubject
			}
		}

		result, err := internal.Tick(internal.TickConfig{
			DB:                 db,
			GWS:                gwsCLI,
			DryRun:             dryRun,
			Timezone:           tz,
			UnsubscribeHeader:  unsubHeader,
			UnsubscribeSubject: unsubSubject,
		})
		if err != nil {
			return err
		}

		// Log tick summary to file
		slog.Info("tick complete",
			"sent", result.Sent, "failed", result.Failed, "skipped", result.Skipped,
			"replies", result.RepliesDetected, "unsubscribes", result.UnsubscribesDetected,
			"bounces", result.BouncesDetected, "dry_run", result.DryRun)

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
		perVariants, _ := cmd.Flags().GetBool("variants")

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

			if perVariants {
				stats, err := internal.GetCampaignVariantStats(db, campaignID)
				if err != nil {
					return err
				}
				if jsonOutput {
					return printJSON(map[string]any{"campaign": name, "variants": stats})
				}
				if len(stats) == 0 {
					fmt.Printf("Campaign %q has no sends yet.\n", name)
					return nil
				}
				fmt.Printf("Campaign: %s\n\n", name)
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "STEP\tVARIANT\tSENT\tREPLIES\tRATE\tUNSUBS\tBOUNCES")
				for _, s := range stats {
					fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%.1f%%\t%d\t%d\n",
						s.Step, s.Variant, s.Sent, s.Replies, s.ReplyRate, s.Unsubscribes, s.Bounces)
				}
				return w.Flush()
			}

			if perLeads {
				stats, err := internal.GetCampaignLeadStats(db, campaignID)
				if err != nil {
					return err
				}
				if jsonOutput {
					return printJSON(map[string]any{"campaign": name, "leads": stats})
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

			stats, err := internal.GetCampaignStepStats(db, campaignID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(map[string]any{"campaign": name, "steps": stats})
			}
			if len(stats) == 0 {
				fmt.Printf("Campaign %q has no events yet.\n", name)
				return nil
			}
			fmt.Printf("Campaign: %s\n\n", name)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STEP\tSENT\tREPLIES\tUNSUBS\tBOUNCES")
			for _, s := range stats {
				fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\n", s.Step, s.Sent, s.Replies, s.Unsubscribes, s.Bounces)
			}
			return w.Flush()
		}

		stats, err := internal.GetAllCampaignStats(db)
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(stats)
		}
		if len(stats) == 0 {
			fmt.Println("No campaigns. Create one with: cold-cli campaign create")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "CAMPAIGN\tSTATUS\tSENT\tREPLIES\tUNSUBS\tBOUNCES")
		for _, s := range stats {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\n", s.Name, s.Status, s.Sent, s.Replies, s.Unsubscribes, s.Bounces)
		}
		return w.Flush()
	},
}

var logCmd = &cobra.Command{
	Use:   "log [campaign]",
	Short: "Show recent activity (sends, replies, bounces, unsubscribes)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		limit, _ := cmd.Flags().GetInt("limit")
		var campaignName string
		if len(args) == 1 {
			campaignName = args[0]
		}

		events, err := internal.GetEventLog(db, campaignName, limit)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(events)
		}

		if len(events) == 0 {
			fmt.Println("No events yet.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TIME\tTYPE\tCAMPAIGN\tLEAD\tACCOUNT\tSTEP")
		for _, e := range events {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n",
				e.Timestamp, e.Type, e.Campaign, e.LeadEmail, e.AccountEmail, e.StepNumber)
		}
		return w.Flush()
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	accountAddCmd.Flags().Int("daily-limit", 50, "maximum emails per day for this account")
	accountAddCmd.Flags().Bool("skip-auth", false, "skip gws OAuth login (for testing or pre-authed accounts)")
	accountCmd.AddCommand(accountAddCmd, accountListCmd, accountPauseCmd, accountResumeCmd, accountRemoveCmd)
	leadListCmd.Flags().String("domain", "", "filter by domain")
	leadListCmd.Flags().String("status", "", "filter by status (active, blacklisted, bounced)")
	leadListCmd.Flags().Int("limit", 50, "max leads to show")
	leadCmd.AddCommand(leadListCmd, leadPauseCmd, leadBlacklistCmd)

	campaignCreateCmd.Flags().String("name", "", "campaign name")
	campaignCreateCmd.Flags().String("sequence", "", "path to sequence YAML file")
	campaignCreateCmd.Flags().String("leads", "", "path to leads CSV file")
	campaignCreateCmd.Flags().String("accounts", "", "comma-separated account emails")
	campaignUpdateCmd.Flags().String("send-window-start", "", "send window start (HH:MM)")
	campaignUpdateCmd.Flags().String("send-window-end", "", "send window end (HH:MM)")
	campaignUpdateCmd.Flags().String("send-days", "", "send days (0=Sun,1=Mon,...,6=Sat)")
	campaignUpdateCmd.Flags().String("timezone", "", "timezone (e.g. America/New_York)")
	campaignUpdateCmd.Flags().Int("min-gap", 0, "minimum seconds between sends")
	campaignUpdateCmd.Flags().Int("max-gap", 0, "maximum seconds between sends")
	campaignCloneCmd.Flags().String("name", "", "new campaign name")
	campaignCloneCmd.Flags().String("leads", "", "path to leads CSV file")
	campaignCloneCmd.Flags().String("accounts", "", "comma-separated account emails (default: reuse source accounts)")
	campaignAddLeadsCmd.Flags().String("leads", "", "path to leads CSV file")
	campaignCmd.AddCommand(campaignCreateCmd, campaignListCmd, campaignPreviewCmd, campaignActivateCmd, campaignPauseCmd, campaignResumeCmd, campaignStatusCmd, campaignDeleteCmd, campaignUpdateCmd, campaignCloneCmd, campaignAddLeadsCmd)

	tickCmd.Flags().Bool("dry-run", false, "show what would be sent without actually sending")

	statsCmd.Flags().Bool("leads", false, "show per-lead breakdown")
	statsCmd.Flags().Bool("variants", false, "show per-variant A/B test results")

	logCmd.Flags().Int("limit", 20, "number of events to show")

	rootCmd.AddCommand(initCmd, accountCmd, leadCmd, campaignCmd, tickCmd, statsCmd, logCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
