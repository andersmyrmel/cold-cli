# cold-cli

Open-source CLI cold email sequence engine. Single binary, SQLite storage, no SaaS.

Built on [gws](https://github.com/nicholasgasior/gws) for Gmail API access. Designed to be operated by coding agents (Claude Code, etc.) or humans comfortable with a terminal.

## Install

```bash
go install github.com/anders/cold-cli/cmd/cold-cli@latest
```

Requires [gws](https://github.com/nicholasgasior/gws) for Gmail integration.

## Quickstart

```bash
# Initialize
cold-cli init

# Add a sending account (opens browser for Google OAuth)
cold-cli account add you@company.com

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
      Last note — just wanted to make sure this didn't get buried.
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

`email` is the only required column. All other columns are driven by what `{{placeholders}}` your sequence uses — cold-cli validates this at campaign creation.

## Commands

```
cold-cli init                              # set up ~/.cold-cli/ directory, database, config
cold-cli account add <email>               # add sending account with OAuth
cold-cli account list                      # list accounts

cold-cli campaign create --name --sequence --leads --accounts
cold-cli campaign preview <name>           # see full schedule before activating
cold-cli campaign activate <name>          # start sending
cold-cli campaign pause <name>             # stop sending
cold-cli campaign resume <name>            # resume
cold-cli campaign status <name>            # details + send counts

cold-cli tick                              # process replies, bounces, send due emails
cold-cli tick --dry-run                    # show what would happen

cold-cli stats [campaign]                  # sent/replied/bounced per campaign
cold-cli stats <name> --leads              # per-lead breakdown

cold-cli lead pause <email>               # pause across all campaigns
cold-cli lead blacklist <email|domain>     # blacklist + cancel pending sends
```

All commands support `--json` for programmatic use.

## How It Works

### Eager Scheduling

All send times are pre-computed when you create a campaign. Each send becomes a row in SQLite with a specific `send_at` timestamp, assigned account, and variant. This means:

- `campaign preview` shows the exact schedule before you activate
- `tick` is trivially simple: query for rows where `send_at <= now`
- Agents can review and approve the full timeline

### Tick Engine

`tick` is a single idempotent command that does everything per invocation:

1. Poll inbox for replies → match via In-Reply-To headers → pause lead
2. Poll inbox for bounces → detect via thread matching → mark bounced
3. Find sends where `send_at <= now` and campaign is active
4. Send each email via gws with 90-140 second random gaps
5. Respect daily limits and send windows

Run it manually, via cron (`*/10 * * * *`), or have an agent call it.

### Reply Detection

Matches inbox messages to sent emails using `In-Reply-To` headers. When a reply is detected, remaining sends for that lead are cancelled. With `stop_on_domain_reply`, all leads on the same domain are paused.

### Bounce Detection

Three-strategy fallback:
1. **Thread matching** — NDR shares a Gmail thread with our sent email (catches all formats)
2. **X-Failed-Recipients header** — standard MTA header
3. **Snippet parsing** — extract bounced email from NDR text

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
```

## Architecture

- **Go** — single binary, no runtime deps
- **SQLite** — `~/.cold-cli/data.db`, pure Go driver (no CGO)
- **gws CLI** — subprocess calls for Gmail API (send, list, get)
- **Cobra** — CLI framework

See [ARCHITECTURE.md](ARCHITECTURE.md) for data model, tick flow diagrams, and design decisions.

## License

MIT
