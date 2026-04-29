package main

import (
	"context"
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

func openStore() (*internal.Store, error) {
	if internal.CurrentDialect() == internal.DialectSQLite {
		dbPath := internal.DBPath()
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("cold-cli not initialized — run 'cold-cli init' first")
		}
	}
	return internal.OpenStore()
}

func openDB() (*sql.DB, error) {
	store, err := openStore()
	if err != nil {
		return nil, err
	}
	return store.DB, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

var rootCmd = &cobra.Command{
	Use:   "cold-cli",
	Short: "Agent-first CLI cold email sequence engine",
	Long: strings.TrimSpace(`
Agent-first CLI cold email sequence engine.

Storage backend:
  - SQLite by default at ~/.cold-cli/data.db
  - Postgres when COLD_CLI_DATABASE_URL is set

For Postgres worker deployments, use a direct connection string rather than a
transaction-pooled/pooler URL because tick uses advisory locks.
`),
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize cold-cli data directory, database, and config",
	Long: strings.TrimSpace(`
Initialize cold-cli data directory, config, and the active database backend.

Backend selection:
  - SQLite by default at ~/.cold-cli/data.db
  - Postgres when COLD_CLI_DATABASE_URL is set
`),
	RunE: func(cmd *cobra.Command, args []string) error {
		dataDir := internal.DataDir()

		if err := internal.EnsureDataDir(); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}

		store, err := internal.OpenStore()
		if err != nil {
			return err
		}
		defer store.Close()

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
				"database": store.DisplayTarget(),
				"config":   configPath,
				"gws_ok":   gwsErr == nil,
			}
			if gwsErr != nil {
				result["gws_error"] = gwsErr.Error()
			}
			return printJSON(result)
		}

		fmt.Printf("Initialized cold-cli at %s\n", dataDir)
		fmt.Printf("  database: %s\n", store.DisplayTarget())
		fmt.Printf("  config:   %s\n", configPath)
		if gwsErr != nil {
			fmt.Printf("  warning:  %s\n", gwsErr)
		} else {
			fmt.Println("  gws:      ok")
		}
		return nil
	},
}

// --- doctor command ---

var doctorCmd = &cobra.Command{
	Use:   "doctor [domain...]",
	Short: "Check domain DNS setup for email deliverability (MX, SPF, DKIM, DMARC)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var domains []string

		if len(args) > 0 {
			domains = args
		} else {
			// Auto-detect from accounts
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()

			seen := map[string]bool{}
			accounts, err := internal.ListAccounts(db)
			if err != nil {
				return err
			}
			for _, a := range accounts {
				parts := strings.SplitN(a.Email, "@", 2)
				if len(parts) == 2 && !seen[parts[1]] {
					seen[parts[1]] = true
					domains = append(domains, parts[1])
				}
			}
			if len(domains) == 0 {
				return fmt.Errorf("no accounts found — specify a domain: cold-cli doctor example.com")
			}
		}

		var allDiags []*internal.DomainDiagnostic
		for _, domain := range domains {
			diag, err := internal.CheckDomain(domain)
			if err != nil {
				return fmt.Errorf("checking %s: %w", domain, err)
			}
			allDiags = append(allDiags, diag)
		}

		if jsonOutput {
			return printJSON(allDiags)
		}

		for i, diag := range allDiags {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Domain: %s\n", diag.Domain)
			for _, c := range diag.Checks {
				if c.Passed {
					fmt.Printf("  ✓ %-6s %s\n", c.Name, c.Detail)
				} else {
					fmt.Printf("  ✗ %-6s %s\n", c.Name, c.Detail)
				}
			}
			fmt.Printf("\n  Score: %d/%d\n", diag.Score, diag.MaxScore)
			for _, c := range diag.Checks {
				if !c.Passed && c.Fix != "" {
					fmt.Printf("  Fix:   %s\n", c.Fix)
				}
			}
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
		skipAuth, _ := cmd.Flags().GetBool("no-login")
		if !skipAuth {
			skipAuth, _ = cmd.Flags().GetBool("skip-auth")
		}

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

		// Auto-check domain deliverability
		parts := strings.SplitN(email, "@", 2)
		if len(parts) == 2 {
			diag, err := internal.CheckDomain(parts[1])
			if err == nil {
				fmt.Println()
				fmt.Printf("Domain check for %s: %d/%d\n", parts[1], diag.Score, diag.MaxScore)
				for _, c := range diag.Checks {
					if !c.Passed {
						fmt.Printf("  ! %-6s %s\n", c.Name, c.Detail)
						if c.Fix != "" {
							fmt.Printf("           Fix: %s\n", c.Fix)
						}
					}
				}
				if diag.Score == diag.MaxScore {
					fmt.Println("  All checks passed.")
				}
			}
		}

		return nil
	},
}

var accountAddSMTPCmd = &cobra.Command{
	Use:   "add-smtp <email>",
	Short: "Add a generic SMTP/IMAP sending account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := strings.TrimSpace(args[0])
		if _, err := mail.ParseAddress(email); err != nil {
			return fmt.Errorf("invalid email address %q", email)
		}

		dailyLimit, _ := cmd.Flags().GetInt("daily-limit")
		smtpHost, _ := cmd.Flags().GetString("smtp-host")
		smtpPort, _ := cmd.Flags().GetInt("smtp-port")
		smtpUser, _ := cmd.Flags().GetString("smtp-user")
		smtpPasswordRef, _ := cmd.Flags().GetString("smtp-password-ref")
		smtpTLS, _ := cmd.Flags().GetString("smtp-tls")
		imapHost, _ := cmd.Flags().GetString("imap-host")
		imapPort, _ := cmd.Flags().GetInt("imap-port")
		imapUser, _ := cmd.Flags().GetString("imap-user")
		imapPasswordRef, _ := cmd.Flags().GetString("imap-password-ref")
		imapTLS, _ := cmd.Flags().GetString("imap-tls")

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.AddSMTPIMAPAccount(db, internal.AddSMTPIMAPAccountOpts{
			Email:           email,
			DailyLimit:      dailyLimit,
			SMTPHost:        smtpHost,
			SMTPPort:        smtpPort,
			SMTPUsername:    smtpUser,
			SMTPPasswordRef: smtpPasswordRef,
			SMTPTLSMode:     smtpTLS,
			IMAPHost:        imapHost,
			IMAPPort:        imapPort,
			IMAPUsername:    imapUser,
			IMAPPasswordRef: imapPasswordRef,
			IMAPTLSMode:     imapTLS,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Added SMTP/IMAP account %s (id=%d, daily_limit=%d)\n", result.Email, result.ID, result.DailyLimit)
		fmt.Printf("  smtp: %s:%d (%s)\n", result.SMTPHost, result.SMTPPort, result.SMTPTLSMode)
		fmt.Printf("  imap: %s:%d (%s)\n", result.IMAPHost, result.IMAPPort, result.IMAPTLSMode)
		fmt.Println()
		fmt.Println("Note: SMTP/IMAP sending is not enabled until the SMTP sender transport is implemented.")
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
		fmt.Fprintln(w, "ID\tEMAIL\tPROVIDER\tDAILY LIMIT\tSTATUS")
		for _, a := range accounts {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\n", a.ID, a.Email, a.Provider, a.DailyLimit, a.Status)
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

var accountUpdateCmd = &cobra.Command{
	Use:   "update <email>",
	Short: "Update account settings (daily limit)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		email := strings.TrimSpace(args[0])
		opts := internal.UpdateAccountOpts{}
		changed := false

		if cmd.Flags().Changed("daily-limit") {
			v, _ := cmd.Flags().GetInt("daily-limit")
			opts.DailyLimit = &v
			changed = true
		}

		if !changed {
			return fmt.Errorf("no settings to update -- use --daily-limit")
		}

		if err := internal.UpdateAccount(db, email, opts); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"email": email, "updated": true})
		}

		fmt.Printf("Updated account %s\n", email)
		if opts.DailyLimit != nil {
			fmt.Printf("  daily limit: %d\n", *opts.DailyLimit)
		}
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

var leadResumeCmd = &cobra.Command{
	Use:   "resume <email>",
	Short: "Resume a paused lead across all campaigns",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.ResumeLead(db, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Resumed %s: %d campaigns resumed, %d sends restored\n",
			result.Email, result.ResumedCampaigns, result.RestoredSends)
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
	Long:  "Create a new campaign from a sequence YAML and leads CSV. Leads may optionally include a schedule_timezone CSV column for per-lead timezone scheduling while still using the campaign send window and send days.",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		seqFile, _ := cmd.Flags().GetString("sequence")
		seqInline, _ := cmd.Flags().GetString("sequence-inline")
		leadsFile, _ := cmd.Flags().GetString("leads")
		leadsInline, _ := cmd.Flags().GetString("leads-inline")
		accountsFlag, _ := cmd.Flags().GetString("accounts")
		startDate, _ := cmd.Flags().GetString("start-date")
		sendDays, _ := cmd.Flags().GetString("send-days")

		if name == "" || accountsFlag == "" {
			return fmt.Errorf("required flags: --name, --accounts")
		}
		if seqFile == "" && seqInline == "" {
			return fmt.Errorf("provide --sequence (file path) or --sequence-inline (YAML content)")
		}
		if leadsFile == "" && leadsInline == "" {
			return fmt.Errorf("provide --leads (file path) or --leads-inline (CSV content)")
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		result, err := internal.CreateCampaign(db, internal.CreateCampaignOpts{
			Name:           name,
			SequenceFile:   seqFile,
			SequenceInline: seqInline,
			LeadsFile:      leadsFile,
			LeadsInline:    leadsInline,
			AccountEmails:  strings.Split(accountsFlag, ","),
			StartDate:      startDate,
			SendDays:       sendDays,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		for _, w := range result.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
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
	Use:   "preview <name|id>",
	Short: "Preview the full send schedule for a campaign",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		render, _ := cmd.Flags().GetBool("render")

		if render {
			leadFilter, _ := cmd.Flags().GetString("lead")
			rendered, err := internal.GetCampaignRenderedPreview(db, name, leadFilter)
			if err != nil {
				return err
			}
			if jsonOutput {
				return printJSON(map[string]any{"campaign": name, "emails": rendered})
			}
			for i, e := range rendered {
				if i > 0 {
					fmt.Println(strings.Repeat("-", 60))
				}
				fmt.Printf("Step %d (variant %d) | %s -> %s\n", e.StepNumber, e.VariantIndex, e.AccountEmail, e.LeadEmail)
				fmt.Printf("Subject: %s\n\n", e.Subject)
				if len(e.StrippedVars) > 0 {
					fmt.Printf("Stripped vars: %s\n\n", strings.Join(e.StrippedVars, ", "))
				}
				fmt.Println(e.Body)
				fmt.Println()
			}
			return nil
		}

		_, status, preview, err := internal.GetCampaignPreview(db, name)
		if err != nil {
			return err
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

		fmt.Printf("Campaign: %s (status: %s, %d sends)\n", name, status, len(preview))
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SEND AT\tSTEP\tVARIANT\tLEAD\tACCOUNT\tSTATUS")
		for _, r := range preview {
			statusCol := r.Status
			if r.Status == "failed" && r.ErrorMessage != "" {
				statusCol = r.Status + "  " + r.ErrorMessage
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n",
				r.SendAt, r.StepNumber, r.VariantIndex, r.LeadEmail, r.AccountEmail, statusCol)
		}
		w.Flush()

		// Show daily limit overflow warnings
		warnings, err := internal.GetDailyLimitWarnings(db)
		if err == nil && len(warnings) > 0 {
			fmt.Println()
			for _, warn := range warnings {
				fmt.Printf("  ! %s: %d sends scheduled for %s, limit is %d (across all campaigns) — %d will defer\n",
					warn.Date, warn.Scheduled, warn.Account, warn.Limit, warn.Overflow)
			}
		}

		return nil
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
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tLEADS\tSENDS\tWINDOW\tDAYS")
		for _, c := range campaigns {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%s\t%s\n", c.ID, c.Name, c.Status, c.Leads, c.Sends, c.SendWindow, c.SendDays)
		}
		return w.Flush()
	},
}

var campaignRemoveLeadCmd = &cobra.Command{
	Use:   "remove-lead <name|id> <email>",
	Short: "Remove a single lead from a campaign",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		result, err := internal.RemoveLeadFromCampaign(db, name, strings.TrimSpace(args[1]))
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		fmt.Printf("Removed %s from campaign %q: %d sends cancelled\n",
			result.Email, result.Campaign, result.CancelledSends)
		return nil
	},
}

var campaignDeleteCmd = &cobra.Command{
	Use:   "delete <name|id>",
	Short: "Delete a campaign and all associated data",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		id, err := internal.DeleteCampaign(db, name)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": name, "id": id, "deleted": true})
		}

		fmt.Printf("Deleted campaign %q (id=%d)\n", name, id)
		return nil
	},
}

var campaignUpdateCmd = &cobra.Command{
	Use:   "update <name|id>",
	Short: "Update campaign settings (sequence, send window, days, gaps)",
	Long:  "Update campaign-level settings such as sequence, send window, send days, timezone, and gaps. Lead-level schedule_timezone overrides from CSV remain lead-specific and are not changed by this command.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

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
		if cmd.Flags().Changed("sequence") {
			v, _ := cmd.Flags().GetString("sequence")
			opts.SequenceFile = &v
			changed = true
		}

		if !changed {
			return fmt.Errorf("no settings to update — use flags like --sequence, --send-days, --send-window-start, etc.")
		}

		if err := internal.UpdateCampaign(db, name, opts); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": name, "updated": true})
		}

		fmt.Printf("Updated campaign %q\n", name)
		return nil
	},
}

var campaignCloneCmd = &cobra.Command{
	Use:   "clone <source-name|id>",
	Short: "Clone a campaign with new leads (copies sequence + settings)",
	Long:  "Clone a campaign with new leads while copying sequence and campaign settings. Leads may optionally include a schedule_timezone CSV column for per-lead timezone scheduling.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		leadsFile, _ := cmd.Flags().GetString("leads")
		leadsInline, _ := cmd.Flags().GetString("leads-inline")
		accountsFlag, _ := cmd.Flags().GetString("accounts")

		if name == "" {
			return fmt.Errorf("required flag: --name")
		}
		if leadsFile == "" && leadsInline == "" {
			return fmt.Errorf("provide --leads (file path) or --leads-inline (CSV content)")
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		sourceName, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		var accounts []string
		if accountsFlag != "" {
			accounts = strings.Split(accountsFlag, ",")
		}

		result, err := internal.CloneCampaign(db, internal.CloneCampaignOpts{
			SourceName:  sourceName,
			NewName:     name,
			LeadsFile:   leadsFile,
			LeadsInline: leadsInline,
			Accounts:    accounts,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		for _, w := range result.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
		}
		fmt.Printf("Cloned %q -> %q (id=%d)\n", sourceName, result.Name, result.ID)
		fmt.Printf("  leads:    %d\n", result.Leads)
		fmt.Printf("  sends:    %d\n", result.ScheduledSends)
		fmt.Printf("  accounts: %d\n", result.Accounts)
		fmt.Printf("  status:   %s\n", result.Status)
		fmt.Printf("\nRun 'cold-cli campaign preview %s' to review the schedule.\n", result.Name)
		return nil
	},
}

var campaignAddLeadsCmd = &cobra.Command{
	Use:   "add-leads <name|id>",
	Short: "Add new leads to an existing campaign",
	Long:  "Add new leads to an existing campaign. Leads may optionally include a schedule_timezone CSV column for per-lead timezone scheduling under the campaign's existing send window and send days.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		leadsFile, _ := cmd.Flags().GetString("leads")
		leadsInline, _ := cmd.Flags().GetString("leads-inline")
		if leadsFile == "" && leadsInline == "" {
			return fmt.Errorf("provide --leads (file path) or --leads-inline (CSV content)")
		}

		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		result, err := internal.AddLeadsToCampaign(db, name, leadsFile, leadsInline)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		for _, w := range result.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
		}
		fmt.Printf("Added leads to %q\n", result.Campaign)
		fmt.Printf("  added:   %d\n", result.LeadsAdded)
		fmt.Printf("  skipped: %d (already in campaign, blacklisted, or bounced)\n", result.LeadsSkipped)
		fmt.Printf("  sends:   %d scheduled\n", result.ScheduledSends)
		return nil
	},
}

var campaignRetryCmd = &cobra.Command{
	Use:   "retry <name|id>",
	Short: "Reset failed sends back to pending so they get retried",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		var step *int
		if cmd.Flags().Changed("step") {
			v, _ := cmd.Flags().GetInt("step")
			step = &v
		}

		result, err := internal.RetryCampaign(db, name, step)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		if result.Retried == 0 {
			fmt.Printf("No failed sends to retry in campaign %q.\n", name)
		} else {
			fmt.Printf("Retried %d failed sends in campaign %q.\n", result.Retried, name)
		}
		return nil
	},
}

var campaignSendNowCmd = &cobra.Command{
	Use:   "send-now <name|id>",
	Short: "Set all pending sends to now so the next tick sends them immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		result, err := internal.SendNowCampaign(db, name)
		if err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(result)
		}

		if result.Updated == 0 {
			fmt.Printf("No pending sends in campaign %q.\n", name)
		} else {
			fmt.Printf("Updated %d pending sends in campaign %q to send now.\n", result.Updated, name)
			fmt.Println("Run 'cold-cli tick' to send them.")
		}
		return nil
	},
}

var campaignActivateCmd = &cobra.Command{
	Use:   "activate <name|id>",
	Short: "Activate a draft campaign so tick will process it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		if err := internal.CampaignStateTransition(db, name, "activate", "draft", "active"); err != nil {
			return err
		}

		sendNow, _ := cmd.Flags().GetBool("send-now")
		var sendNowResult *internal.SendNowResult
		if sendNow {
			sendNowResult, err = internal.SendNowCampaign(db, name)
			if err != nil {
				return err
			}
		}

		if jsonOutput {
			out := map[string]any{"name": name, "status": "active"}
			if sendNowResult != nil {
				out["send_now"] = sendNowResult.Updated
			}
			return printJSON(out)
		}

		fmt.Printf("Campaign %q is now active.\n", name)
		if sendNowResult != nil && sendNowResult.Updated > 0 {
			fmt.Printf("Updated %d pending sends to send now.\n", sendNowResult.Updated)
			fmt.Println("Run 'cold-cli tick' to send them.")
		}
		return nil
	},
}

var campaignPauseCmd = &cobra.Command{
	Use:   "pause <name|id>",
	Short: "Pause an active campaign",
	Args:  cobra.ExactArgs(1),
	RunE:  campaignStateCmd("pause", "active", "paused"),
}

var campaignResumeCmd = &cobra.Command{
	Use:   "resume <name|id>",
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

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		if err := internal.CampaignStateTransition(db, name, action, from, to); err != nil {
			return err
		}

		if jsonOutput {
			return printJSON(map[string]any{"name": name, "status": to})
		}

		fmt.Printf("Campaign %q is now %s.\n", name, to)
		return nil
	}
}

var campaignStatusCmd = &cobra.Command{
	Use:   "status <name|id>",
	Short: "Show campaign details and send counts by status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		db, err := openDB()
		if err != nil {
			return err
		}
		defer db.Close()

		name, err := internal.ResolveCampaignName(db, args[0])
		if err != nil {
			return err
		}

		info, err := internal.GetCampaignStatus(db, name)
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
		fmt.Printf("  send days:   %s\n", info.SendDays)
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
		if len(info.FailureReasons) > 0 {
			fmt.Printf("\nFailure reasons:\n")
			for _, fr := range info.FailureReasons {
				fmt.Printf("  %s (%d sends)\n", fr.Error, fr.Count)
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
		sendNow, _ := cmd.Flags().GetBool("now")

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
		}

		store, err := openStore()
		if err != nil {
			return err
		}
		defer store.Close()

		if !dryRun {
			lock, err := store.AcquireTickLock(context.Background())
			if err != nil {
				if jsonOutput {
					return printJSON(map[string]any{"status": "locked", "message": "tick already running"})
				}
				fmt.Println("tick already running")
				return nil
			}
			defer lock.Close()
		}

		db := store.DB

		// Load timezone for daily limit calculation
		cfg, _ := internal.LoadConfig()
		var tz *time.Location
		if cfg != nil {
			tz, _ = time.LoadLocation(cfg.DefaultTimezone)
		}

		gwsCLI := internal.NewGWSCLI()
		rows, err := store.Query("SELECT email, gws_config_dir FROM accounts WHERE status = 'active' AND provider = 'gws' AND gws_config_dir != ''")
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
			SendNow:            sendNow,
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
	Short: "Show send/reply/bounce statistics (per-campaign when name given, global otherwise)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := openStore()
		if err != nil {
			return err
		}
		defer store.Close()
		db := store.DB

		perLeads, _ := cmd.Flags().GetBool("leads")
		perVariants, _ := cmd.Flags().GetBool("variants")

		if len(args) == 1 {
			name, err := internal.ResolveCampaignName(db, args[0])
			if err != nil {
				return err
			}
			var campaignID int64
			err = store.QueryRow("SELECT id FROM campaigns WHERE name = ?", name).Scan(&campaignID)
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
			resolved, err := internal.ResolveCampaignName(db, args[0])
			if err != nil {
				return err
			}
			campaignName = resolved
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

var campaignInitCmd = &cobra.Command{
	Use:   "init [directory]",
	Short: "Scaffold example sequence.yml and leads.csv files",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) == 1 {
			dir = args[0]
		}

		seqPath := filepath.Join(dir, "sequence.yml")
		leadsPath := filepath.Join(dir, "leads.csv")

		// Check for existing files
		for _, p := range []string{seqPath, leadsPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists — remove it first or use a different directory", p)
			}
		}

		if dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("creating directory: %w", err)
			}
		}

		seqContent := `name: example-sequence

defaults:
  from_name: Your Name

steps:
  - step: 1
    delay: 0
    subject: "Quick question, {{first_name}}"
    body: |
      Hi {{first_name}},

      I noticed {{company}} and wanted to reach out.

      Would you be open to a quick chat this week?

      Best,
      Your Name

  - step: 2
    delay: 3
    subject: ""
    body: |
      Hi {{first_name}},

      Just bumping this to the top of your inbox. Would love to connect.

      Best,
      Your Name

  - step: 3
    delay: 5
    subject: ""
    body: |
      Hi {{first_name}},

      Last follow-up. If the timing isn't right, no worries at all.

      Best,
      Your Name
`
		if err := os.WriteFile(seqPath, []byte(seqContent), 0644); err != nil {
			return fmt.Errorf("writing sequence file: %w", err)
		}

		leadsContent := `email,first_name,last_name,company,schedule_timezone
alice@example.com,Alice,Smith,Acme Corp,America/New_York
bob@example.com,Bob,Jones,Widget Inc,Europe/Oslo
`
		if err := os.WriteFile(leadsPath, []byte(leadsContent), 0644); err != nil {
			return fmt.Errorf("writing leads file: %w", err)
		}

		if jsonOutput {
			return printJSON(map[string]any{
				"sequence": seqPath,
				"leads":    leadsPath,
			})
		}

		fmt.Printf("Created example files:\n")
		fmt.Printf("  %s  — edit your email sequence here\n", seqPath)
		fmt.Printf("  %s     — add your leads here (optional: schedule_timezone)\n", leadsPath)
		fmt.Printf("\nThen create a campaign:\n")
		fmt.Printf("  cold-cli campaign create --name my-campaign --sequence %s --leads %s --accounts you@gmail.com\n", seqPath, leadsPath)
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	accountAddCmd.Flags().Int("daily-limit", 50, "max emails per day, shared across all campaigns using this account")
	accountAddCmd.Flags().Bool("no-login", false, "skip OAuth login (use when gws is already authenticated)")
	accountAddCmd.Flags().Bool("skip-auth", false, "skip OAuth login (alias for --no-login)")
	accountAddCmd.Flags().MarkHidden("skip-auth")
	accountAddSMTPCmd.Flags().Int("daily-limit", 50, "max emails per day, shared across all campaigns using this account")
	accountAddSMTPCmd.Flags().String("smtp-host", "", "SMTP server hostname")
	accountAddSMTPCmd.Flags().Int("smtp-port", 0, "SMTP server port (default depends on --smtp-tls)")
	accountAddSMTPCmd.Flags().String("smtp-user", "", "SMTP username (default: account email)")
	accountAddSMTPCmd.Flags().String("smtp-password-ref", "", "SMTP password reference, such as env:MIGADU_SMTP_PASSWORD")
	accountAddSMTPCmd.Flags().String("smtp-tls", "ssl", "SMTP TLS mode: ssl, starttls, none")
	accountAddSMTPCmd.Flags().String("imap-host", "", "IMAP server hostname")
	accountAddSMTPCmd.Flags().Int("imap-port", 0, "IMAP server port (default depends on --imap-tls)")
	accountAddSMTPCmd.Flags().String("imap-user", "", "IMAP username (default: SMTP username)")
	accountAddSMTPCmd.Flags().String("imap-password-ref", "", "IMAP password reference (default: SMTP password ref)")
	accountAddSMTPCmd.Flags().String("imap-tls", "ssl", "IMAP TLS mode: ssl, starttls, none")
	accountUpdateCmd.Flags().Int("daily-limit", 0, "max emails per day, shared across all campaigns using this account")
	accountCmd.AddCommand(accountAddCmd, accountAddSMTPCmd, accountListCmd, accountPauseCmd, accountResumeCmd, accountRemoveCmd, accountUpdateCmd)
	leadListCmd.Flags().String("domain", "", "filter by domain")
	leadListCmd.Flags().String("status", "", "filter by status (active, blacklisted, bounced)")
	leadListCmd.Flags().Int("limit", 50, "max leads to show")
	leadCmd.AddCommand(leadListCmd, leadPauseCmd, leadResumeCmd, leadBlacklistCmd)

	campaignCreateCmd.Flags().String("name", "", "campaign name")
	campaignCreateCmd.Flags().String("sequence", "", "path to sequence YAML file")
	campaignCreateCmd.Flags().String("sequence-inline", "", "sequence YAML content (alternative to --sequence)")
	campaignCreateCmd.Flags().String("leads", "", "path to leads CSV file (optional per-lead schedule_timezone column supported)")
	campaignCreateCmd.Flags().String("leads-inline", "", "leads CSV content (alternative to --leads; optional per-lead schedule_timezone column supported)")
	campaignCreateCmd.Flags().String("accounts", "", "comma-separated account emails")
	campaignCreateCmd.Flags().String("start-date", "", "start date (YYYY-MM-DD); default: tomorrow")
	campaignCreateCmd.Flags().String("send-days", "", "campaign send days override: numbers (0=Sun,1=Mon,...,6=Sat) or names (mon,tue,wed)")
	campaignPreviewCmd.Flags().Bool("render", false, "show rendered email content with templates filled in, including stripped placeholder warnings")
	campaignPreviewCmd.Flags().String("lead", "", "show rendered preview for a specific lead email (use with --render)")
	campaignUpdateCmd.Flags().String("sequence", "", "path to new sequence YAML file")
	campaignUpdateCmd.Flags().String("send-window-start", "", "send window start (HH:MM)")
	campaignUpdateCmd.Flags().String("send-window-end", "", "send window end (HH:MM)")
	campaignUpdateCmd.Flags().String("send-days", "", "send days: numbers (0=Sun,1=Mon,...,6=Sat) or names (mon,tue,wed)")
	campaignUpdateCmd.Flags().String("timezone", "", "campaign default timezone (e.g. America/New_York); leads with schedule_timezone keep their override")
	campaignUpdateCmd.Flags().Int("min-gap", 0, "minimum seconds between sends")
	campaignUpdateCmd.Flags().Int("max-gap", 0, "maximum seconds between sends")
	campaignCloneCmd.Flags().String("name", "", "new campaign name")
	campaignCloneCmd.Flags().String("leads", "", "path to leads CSV file (optional per-lead schedule_timezone column supported)")
	campaignCloneCmd.Flags().String("leads-inline", "", "leads CSV content (alternative to --leads; optional per-lead schedule_timezone column supported)")
	campaignCloneCmd.Flags().String("accounts", "", "comma-separated account emails (default: reuse source accounts)")
	campaignAddLeadsCmd.Flags().String("leads", "", "path to leads CSV file (optional per-lead schedule_timezone column supported)")
	campaignAddLeadsCmd.Flags().String("leads-inline", "", "leads CSV content (alternative to --leads; optional per-lead schedule_timezone column supported)")
	campaignRetryCmd.Flags().Int("step", 0, "only retry failed sends for this step number")
	campaignActivateCmd.Flags().Bool("send-now", false, "set all pending sends to now so they send immediately")
	campaignCmd.AddCommand(campaignCreateCmd, campaignListCmd, campaignPreviewCmd, campaignActivateCmd, campaignPauseCmd, campaignResumeCmd, campaignStatusCmd, campaignDeleteCmd, campaignRemoveLeadCmd, campaignUpdateCmd, campaignCloneCmd, campaignAddLeadsCmd, campaignInitCmd, campaignRetryCmd, campaignSendNowCmd)

	tickCmd.Flags().Bool("dry-run", false, "show what would be sent without actually sending")
	tickCmd.Flags().Bool("now", false, "ignore send_at timestamps and send all pending emails immediately")

	statsCmd.Flags().Bool("leads", false, "show per-lead breakdown")
	statsCmd.Flags().Bool("variants", false, "show per-variant A/B test results")

	logCmd.Flags().Int("limit", 20, "number of events to show")

	rootCmd.AddCommand(initCmd, doctorCmd, accountCmd, leadCmd, campaignCmd, tickCmd, statsCmd, logCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
