package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"text/tabwriter"

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

		result, err := db.Exec(
			"INSERT INTO accounts (email, daily_limit) VALUES (?, ?)",
			email, dailyLimit,
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
				"id":          id,
				"email":       email,
				"daily_limit": dailyLimit,
				"status":      "active",
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

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	accountAddCmd.Flags().Int("daily-limit", 50, "maximum emails per day for this account")
	accountCmd.AddCommand(accountAddCmd, accountListCmd)
	leadCmd.AddCommand(leadPauseCmd, leadBlacklistCmd)

	rootCmd.AddCommand(initCmd, accountCmd, leadCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
