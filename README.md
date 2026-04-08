# cold-cli

Open-source CLI cold email sequence engine. Single binary, SQLite storage, no SaaS.

Built on [gws](https://github.com/googleworkspace/cli) for Gmail API access. Works great with coding agents (Claude Code, Cursor, etc.) or directly from the terminal.

## Install

```bash
go install github.com/anders/cold-cli/cmd/cold-cli@latest
```

Requires [gws](https://github.com/googleworkspace/cli) for Gmail integration.

## Quickstart

```bash
# Initialize
cold-cli init

# Check domain deliverability
cold-cli doctor

# Add a sending account (opens browser for Google OAuth)
cold-cli account add you@company.com

# Scaffold example sequence + leads files (optional)
cold-cli campaign init

# Create a campaign
cold-cli campaign create \
  --name "q1-outreach" \
  --sequence sequence.yml \
  --leads leads.csv \
  --accounts you@company.com

# Review the full schedule before sending anything
cold-cli campaign preview q1-outreach

# Activate when ready
cold-cli campaign activate q1-outreach

# Send due emails (run manually or via cron)
cold-cli tick

# Check results
cold-cli stats q1-outreach
cold-cli stats q1-outreach --variants    # A/B test results
cold-cli log                              # recent activity
```

## Sequence Format

Sequences are YAML files with steps, delays, and optional A/B variants:

```yaml
name: Q1 Agency Outreach
defaults:
  from_name: "Anders"
steps:
  - step: 1
    delay: 0
    subject: "{{first_name}}, quick question about {{company}}"
    body: |
      Hi {{first_name}},

      Saw that {{company}} is growing fast...
    variants:
      - subject: "{{company}} + lifecycle emails"
        body: |
          Hi {{first_name}}, wanted to reach out...
  - step: 2
    delay: 3
    body: |
      Hey {{first_name}}, circling back...
  - step: 3
    delay: 5
    body: |
      Last note - just wanted to make sure this didn't get buried.
```

- `delay` is in days after the previous step
- Steps without a `subject` send as replies in the same thread
- `{{placeholders}}` are replaced from CSV columns
- `variants` enable A/B testing (assigned per lead at creation)

## Leads CSV

```csv
email,first_name,company
john@acme.com,John,Acme Inc
jane@bigcorp.com,Jane,BigCorp
```

`email` is the only required column. All other columns are driven by what `{{placeholders}}` your sequence uses. Extra columns beyond the built-in fields (`first_name`, `last_name`, `company`) are stored as custom fields and available for templates at send time.

- **Validation at creation** - mismatched variables produce actionable errors with "Did you mean?" suggestions
- **Aliases** - common names like `{{name}}` → `first_name` are resolved automatically
- **Reserved names blocked** - CSV columns named `subject`, `body`, `step`, `delay`, or `variant` are rejected (they conflict with sequence YAML fields)
- **Reimport updates** - if a lead already exists, its fields are updated from the new CSV (not silently skipped)
- **Safety at send time** - unresolved variables are stripped (never sent literally); emails with empty subject or body are not sent

## Commands

```
cold-cli init                              # set up ~/.cold-cli/ directory, database, config
cold-cli doctor [domain...]                # check MX, SPF, DKIM, DMARC, domain age

cold-cli account add <email>               # add sending account with OAuth
cold-cli account add <email> --no-login    # add without OAuth (already authed)
cold-cli account list                      # list accounts
cold-cli account update <email>            # update settings (--daily-limit)
cold-cli account pause <email>             # deactivate, cancel pending sends
cold-cli account resume <email>            # reactivate a paused account
cold-cli account remove <email>            # deactivate (re-add later with account add)

cold-cli campaign init [directory]         # scaffold example sequence.yml + leads.csv
cold-cli campaign create --name --sequence --leads --accounts [--start-date YYYY-MM-DD] [--send-days "1,2,3,4,5"]
cold-cli campaign create --name --sequence-inline '...' --leads-inline '...' --accounts  # no files needed
cold-cli campaign clone <source> --name <new> --leads <csv>
cold-cli campaign add-leads <name|id> --leads <csv>    # or --leads-inline '...'
cold-cli campaign remove-lead <name|id> <email>        # remove one lead from a campaign
cold-cli campaign preview <name|id>        # see full schedule before activating
cold-cli campaign preview <name|id> --render  # see rendered emails for first lead
cold-cli campaign preview <name|id> --render --lead <email>  # render for specific lead
cold-cli campaign activate <name|id>       # start sending
cold-cli campaign activate <name|id> --send-now  # activate and send immediately
cold-cli campaign send-now <name|id>       # set all pending sends to now
cold-cli campaign pause <name|id>          # stop sending
cold-cli campaign resume <name|id>         # resume
cold-cli campaign status <name|id>         # details + reply rate + next/last send
cold-cli campaign list                     # list all campaigns (with send window + days)
cold-cli campaign update <name|id>         # update sequence, send window/days, timezone, gaps
cold-cli campaign update <name|id> --send-days "0,1,2,3,4,5,6"  # reschedule pending sends only
cold-cli campaign delete <name|id>         # delete campaign and all data
cold-cli campaign retry <name|id>          # reset failed sends back to pending
cold-cli campaign retry <name|id> --step N # retry only failed sends for step N

cold-cli tick                              # process replies, bounces, send due emails
cold-cli tick --dry-run                    # show what would happen
cold-cli tick --now                        # ignore schedule, send all pending immediately

cold-cli stats [campaign]                  # sent/replied/bounced per campaign
cold-cli stats <name> --leads             # per-lead breakdown
cold-cli stats <name> --variants          # A/B test results with reply rates

cold-cli log [campaign]                    # recent activity (sends, replies, bounces)
cold-cli log --limit 50                    # show more events

cold-cli lead list                         # list all leads
cold-cli lead list --domain <domain>       # filter by domain
cold-cli lead list --status <status>       # filter by status
cold-cli lead pause <email>                # pause across all campaigns
cold-cli lead resume <email>              # undo pause, restore pending sends
cold-cli lead blacklist <email|domain>     # blacklist + cancel pending sends
```

All commands support `--json` for programmatic use.

## How It Works

### Eager Scheduling

All send times are pre-computed when you create a campaign. Each send becomes a row in SQLite with a specific `send_at` timestamp, assigned account, and variant. This means:

- `campaign preview` shows the sender-capacity-aware schedule before you activate
- schedules are rebalanced across `active` and `draft` campaigns that share an account
- `tick` uses the same rebalance logic as preview before loading due rows
- Agents can review and approve the full timeline
- `campaign update --send-days/--send-window-*/--timezone` recalculates existing `pending` sends without touching `sent`, `failed`, `skipped`, or `cancelled` rows
- For leads with no sent history, update recomputes the first pending send from `max(now, campaign start date)` under the new window/day/timezone rules, then chains later pending sends from that new anchor
- For leads already in flight, update preserves sent history and only reschedules future pending sends
- If a prior step is actually sent later than planned, future pending follow-ups are re-anchored from the actual `sent_at` so configured delays still hold

### Tick Engine

`tick` is a single idempotent command that does everything per invocation:

1. Poll inbox for replies → match via In-Reply-To headers → mark lead replied
2. Poll inbox for bounces → detect via thread matching → mark bounced
3. Detect unsubscribe requests → auto-blacklist lead globally
4. Rebalance pending sends for the affected sender accounts using real daily-limit capacity
5. Find sends where `send_at <= now` and campaign is active
6. Re-check each pending row just before send so stale preloaded rows cannot fire
7. Send each email via gws with 90-140 second random gaps
8. After each successful send, rebalance that sender again so future follow-ups chain from actual send time
9. Respect daily limits, send windows, and send days

Run it manually, via cron (`*/10 * * * *`), or have an agent call it. All tick activity is logged to `~/.cold-cli/tick.log` as structured JSON.

### Reply & Unsubscribe Detection

Matches inbox messages to sent emails using `In-Reply-To` headers. When a reply is detected, the lead is marked `replied` and remaining sends for that lead are cancelled. With `stop_on_domain_reply`, all other leads on the same domain are paused.

Unsubscribe requests ("unsubscribe", "remove me", "opt out", etc.) are auto-detected and blacklist the lead globally across all campaigns.

### Bounce Detection

Three-strategy fallback:
1. **Thread matching** - NDR shares a Gmail thread with our sent email (catches all formats)
2. **X-Failed-Recipients header** - standard MTA header
3. **Snippet parsing** - extract bounced email from NDR text

### Multi-Account

Each account gets its own OAuth credentials. Campaigns can use one account or rotate across multiple:

```bash
cold-cli account add sender1@company.com
cold-cli account add sender2@company.com

# Single account
cold-cli campaign create --accounts sender1@company.com ...

# Round-robin across accounts
cold-cli campaign create --accounts sender1@company.com,sender2@company.com ...
```

When round-robin is used, all steps for a given lead use the same account (required for Gmail thread continuity).

### Campaign Cloning

Clone a campaign with new leads. Copies sequence, settings, and accounts:

```bash
cold-cli campaign clone q1-outreach --name q2-outreach --leads new-leads.csv
```

Add more leads to a running campaign:

```bash
cold-cli campaign add-leads q1-outreach --leads more-leads.csv
```

Automatically skips leads already in the campaign, blacklisted, or bounced.

### Domain Diagnostics

Check your sending domains for deliverability issues:

```bash
cold-cli doctor              # auto-checks all account domains
cold-cli doctor example.com  # check specific domain
```

Checks MX records, SPF, DKIM (19 common selectors), DMARC, and domain age via WHOIS.

## Configuration

`~/.cold-cli/config.yml`:

```yaml
default_timezone: America/New_York
default_daily_limit: 50
min_gap_seconds: 90
max_gap_seconds: 140
send_window_start: "09:00"
send_window_end: "17:00"
send_days: "1,2,3,4,5"

# Unsubscribe reply detection is always on.
# List-Unsubscribe header is off by default (not needed for cold email from personal Gmail).
unsubscribe_header: false
unsubscribe_subject: Unsubscribe
```

`send_days` in config is the default for new campaigns. Override it per campaign with `cold-cli campaign create --send-days ...`.

## Architecture

- **Go** - single binary, no runtime deps
- **SQLite** - `~/.cold-cli/data.db`, pure Go driver (no CGO)
- **gws CLI** - subprocess calls for Gmail API (send, list, get)
- **Cobra** - CLI framework
- **log/slog** - structured JSON logging to `~/.cold-cli/tick.log`

See [ARCHITECTURE.md](ARCHITECTURE.md) for data model, tick flow diagrams, and design decisions.

## License

MIT
