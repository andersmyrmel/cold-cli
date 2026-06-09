# cold-cli Architecture

Open-source, agent-first CLI cold email sequence engine in Go. Supports Google Workspace/Gmail through [gws CLI](https://github.com/googleworkspace/cli) and generic email hosts through native SMTP/IMAP transports.

## Problem

No CLI-native cold email sequence engine exists. SaaS tools (Instantly, Smartlead, Lemlist) are all GUIs. We want the sequence engine layer only, with provider transports handling mail send/receive.

## Tech Stack

- **Go** single binary, no runtime deps
- **SQLite or Postgres** storage
  - SQLite local mode: `~/.cold-cli/data.db`, pure Go driver (`modernc.org/sqlite`, no CGO)
  - Postgres shared/server mode: activated by `COLD_CLI_DATABASE_URL`
- **gws CLI** subprocess calls for Google Workspace/Gmail send + inbox polling
- **SMTP/IMAP** native transports for generic email hosts
- **Cobra** CLI framework (same as gh, docker, kubectl)
- **YAML** sequence definitions and config (`~/.cold-cli/config.yml`)
- **log/slog** structured JSON logging to `~/.cold-cli/tick.log`
- **whois** domain age lookups for `cold-cli doctor`

## Project Structure

```
cold-cli/
├─ cmd/cold-cli/
│   └─ main.go              (CLI entry, Cobra commands)
├─ internal/
│   ├─ db.go                (schema bootstrap, SQLite migrations, indexes)
│   ├─ store.go             (dialect-aware open, backend selection, tick locking)
│   ├─ sql_runner.go        (cross-dialect query execution + placeholder rebinding)
│   ├─ models.go            (structs: Account, Lead, Campaign, etc.)
│   ├─ tick.go              (tick engine: lock, poll, send loop)
│   ├─ scheduler.go         (eager schedule computation, variant assignment)
│   ├─ gws.go               (GWSClient interface + real subprocess impl)
│   ├─ smtp.go              (generic SMTP sender + verifier)
│   ├─ imap.go              (generic IMAP polling + verifier)
│   ├─ send.go              (email construction: RFC 2822, threading, List-Unsubscribe)
│   ├─ reply.go             (reply/bounce/unsubscribe detection, header matching)
│   ├─ template.go          ({{placeholder}} string replacement)
│   ├─ csv.go               (lead CSV import, BOM stripping, validation)
│   ├─ config.go            (YAML config loading)
│   ├─ stats.go             (campaign/step/variant/lead stats, event log)
│   ├─ account.go           (account CRUD, provider config, domain diagnostics)
│   ├─ account_verify.go    (SMTP/IMAP account verification)
│   ├─ lead.go              (lead pause/resume/blacklist/list, campaign remove-lead)
│   └─ campaign.go          (campaign CRUD, clone, add-leads, inline creation)
├─ go.mod
└─ go.sum
```

## Data Model

```
accounts
├─ id
├─ workspace_id
├─ email
├─ daily_limit
├─ last_send_at
├─ status (active/paused/removed)
├─ provider (gws/smtp_imap)
├─ gws_config_dir
├─ smtp_host, smtp_port, smtp_username, smtp_password_ref, smtp_tls_mode
└─ imap_host, imap_port, imap_username, imap_password_ref, imap_tls_mode

campaigns
├─ id, workspace_id, name, status, sequence_file, sequence_content
├─ sequence_content (YAML stored at creation time)
├─ stop_on_reply, stop_on_domain_reply
├─ send_window_start/end, send_days, timezone
├─ min_gap_seconds, max_gap_seconds
└─ created_at

campaign_accounts
├─ campaign_id
└─ account_id

leads
├─ id, email, first_name, last_name, company, domain
├─ custom_fields (json)
│  └─ optional scheduling override: `schedule_timezone`
├─ global_status (active/blacklisted/bounced)
└─ created_at

campaign_leads
├─ campaign_id, lead_id
├─ status (active/replied/bounced/paused)
└─ started_at

scheduled_sends
├─ id
├─ campaign_id, lead_id, account_id
├─ step_number, variant_index
├─ send_at                    (pre-computed at campaign creation)
├─ status                     (pending/sent/skipped/cancelled/failed)
├─ thread_id                  (backfilled after step 1 send)
├─ parent_message_id          (backfilled after step 1 send)
├─ message_id                 (filled after send)
├─ sent_at                    (filled after send)
└─ error_message              (filled when status = 'failed')

events
├─ id, campaign_id, lead_id, account_id
├─ type (sent/reply/bounce/unsubscribe/failed)
├─ step_number, message_id, thread_id
├─ timestamp
└─ metadata (json)

kv
├─ key (e.g. last_poll_at)
└─ value
```

### Indexes

```sql
CREATE INDEX idx_sends_pending ON scheduled_sends(status, send_at) WHERE status = 'pending';
CREATE INDEX idx_events_account_day ON events(account_id, type, timestamp);
CREATE INDEX idx_events_message_id ON events(message_id);
CREATE INDEX idx_leads_email ON leads(email);
CREATE INDEX idx_leads_domain ON leads(domain);
```

### scheduled_sends Status State Machine

```
┌─────────┐
│ pending │──send ok────▶ sent
│         │──send fail──▶ failed
│         │──reply──────▶ skipped
│         │──bounce─────▶ skipped
│         │──unsub──────▶ cancelled (via blacklist)
│         │──user───────▶ cancelled
└─────────┘
```

## Core Engine: tick

Single idempotent command. Triggered by cron (`*/10 * * * *`), manual invocation, or agent. All output logged to `~/.cold-cli/tick.log` as structured JSON.

```
tick starts
│
├─ acquire dialect-specific tick lock
│   ├─ SQLite: ~/.cold-cli/tick.lock (flock/fcntl)
│   └─ Postgres: advisory lock on a dedicated DB connection
│      locked? → print "tick already running", exit 0
│
├─ 1. Poll inboxes through provider transports
│      ├─ gws accounts: Gmail queries after last_poll_at
│      └─ smtp_imap accounts: IMAP messages after last_poll_at
│      → match replies via In-Reply-To header, then thread-ID fallback
│      → detect unsubscribe requests (keyword matching)
│      │   → unsubscribe: blacklist lead globally, cancel all sends
│      │   → reply: UPDATE campaign_leads.status = 'replied',
│      │            skip remaining sends
│      → if stop_on_domain_reply: skip same-domain leads in campaign
│      → structured JSON logging via log/slog for all operations
│
├─ 2. Poll inboxes for bounce NDRs
│      → extract bounced email, match to leads
│      → UPDATE leads.global_status = 'bounced'
│      → UPDATE scheduled_sends.status = 'skipped'
│
├─ 3. Preload daily send counts per account
│      SELECT account_id, COUNT(*) FROM events
│      WHERE type='sent' AND timestamp >= start_of_today_in_config_tz
│      GROUP BY account_id
│      (uses config default_timezone for correct day boundary)
│
├─ 4. Rebalance pending scheduled_sends for active/draft campaigns
│      sharing each active account
│      - use account daily_limit capacity derived from sent events
│      - preserve step delays per lead
│      - for in-flight leads, anchor future follow-ups from actual sent_at
│
├─ 5. SELECT * FROM scheduled_sends
│     WHERE send_at <= now AND status = 'pending'
│     (campaign must be active; send window + send day still checked at tick)
│
├─ 6. For each send:
│      ├─ re-read scheduled_sends row before sending
│      │   (skip if no longer pending or rebalanced into the future)
│      ├─ load sequence from DB content (fallback to file for pre-migration)
│      ├─ load lead fields including custom_fields JSON
│      ├─ render template (strings.ReplaceAll)
│      ├─ construct RFC 2822 message
│      │   step 1: new thread (Subject, From, To)
│      │   step 2+: In-Reply-To, References, Re: Subject, thread_id
│      │   optional: List-Unsubscribe header (if configured)
│      ├─ call provider sender (gws or SMTP, 30s timeout)
│      │   success → mark 'sent', INSERT event (error-checked + logged)
│      │   failure → mark 'failed', slog.Error, continue
│      ├─ validate message_id/thread_id returned (else mark failed)
│      ├─ if step 1: backfill thread_id + parent_message_id
│      │   onto all future scheduled_sends for this lead+campaign
│      ├─ rebalance that account again so future pending sends
│      │   inherit the actual sent_at anchor
│      ├─ increment in-memory daily count
│      └─ sleep 90-140 sec (random)
│
├─ 7. Log summary to tick.log, print human-readable summary
│
└─ release lock
```

## Eager Scheduling

All send times are stored eagerly in `scheduled_sends`, then deterministically rebalanced for sender capacity whenever schedule reality changes. Each send remains a concrete row with a concrete `send_at`.

Enables:
- `campaign preview` to show a realistic, sender-capacity-aware schedule before activating
- Agent review of the timeline
- tick, preview, and daily-limit warnings all use the same rebalance logic
- `campaign update` can recalculate `pending` rows in place when send days/window/timezone change
- Unsent leads get a fresh first-pending anchor from `max(now, start_date)` under the updated window/day/timezone rules; in-flight leads keep their sent-history anchor

### Schedule Computation

At `campaign create` (or `clone` / `add-leads`):
1. Parse sequence YAML (steps, delays, variants)
2. Parse leads CSV (validate `email` + all `{{placeholders}}` used in sequence)
3. Assign accounts round-robin (all steps for one lead = same account for thread continuity)
4. Assign variants (round-robin across leads for each step that has variants)
5. Compute `send_at` for each lead+step:
   - Step 1: campaign start time + offset based on lead position
   - Optional lead override: `schedule_timezone` in CSV/custom fields overrides the campaign timezone for that lead only
   - Step N: previous step's current anchor + delay days
   - Clamp to send window (start/end hours)
   - Skip non-send days (e.g., weekends)
   - Add jitter within min/max gap range
6. INSERT all `scheduled_sends` rows with status='pending'
7. Rebalance pending sends across all active/draft campaigns sharing each account so daily limits are already reflected in preview

Notes:
- `schedule_timezone` is backward-compatible because campaign `timezone` remains the default for leads without an override.
- Send window and send days are still campaign-level settings; they are interpreted in each lead's effective timezone.
- If leads need materially different local windows, split campaigns by geography for now.

### Catch-Up After Laptop Sleep

tick processes all overdue sends (`send_at <= now`) with normal 90-140 sec gaps. Daily limit, send window, and send day are the safety valves. No staleness cutoff. Recipients don't see the originally scheduled time.

## Account Rotation

Round-robin assignment at initial schedule time (not dynamic per send). All steps for a given lead use the same account so follow-ups keep provider-specific thread/message continuity. Account-aware rebalance can move timestamps across days, but never reassigns a lead to a different account.

## Reply/Bounce/Unsubscribe Handling

- **Reply detected** → `campaign_leads.status = 'replied'`, remaining `scheduled_sends` marked `'skipped'`. Two matching strategies: (1) `In-Reply-To` header → sent `message_id` (primary, precise), (2) provider thread/message fallback (catches replies from shared inboxes, forwarded addresses, or mail clients that don't set `In-Reply-To`)
- **Domain reply** (if `stop_on_domain_reply=true`) → all leads with same domain in that campaign get their pending sends skipped
- **Unsubscribe detected** (keyword matching: "unsubscribe", "remove me", "opt out", etc.) → lead blacklisted globally, all pending sends across all campaigns cancelled, `'unsubscribe'` event recorded
- **Bounce detected** → `leads.global_status = 'bounced'` (global), `campaign_leads.status = 'bounced'`, pending sends skipped
- Daily send counts derived from `COUNT(*) FROM events WHERE type='sent'` with timezone-aware day boundary
- When a send drifts later than planned, future pending rows for that lead are re-anchored from actual `sent_at`, not the stale planned time

## Template Engine

Simple `strings.ReplaceAll` for `{{placeholder}}` substitution. No template engine, no injection risk.

- Placeholders validated at campaign creation: extract all `{{X}}` from sequence YAML, verify every lead has non-empty values
- Common aliases auto-resolved: `{{name}}` → `first_name`, `{{firstname}}` → `first_name`, `{{last}}` → `last_name`, etc.
- Unknown placeholders produce actionable errors with available field list and Levenshtein "Did you mean?" suggestions
- CSV schema: `email` is the only hardcoded required column; all other required columns are driven by the sequence's placeholders
- CSV column aliases auto-mapped: a `name` column becomes `first_name` (unless `first_name` already exists)
- Reserved CSV column names (`subject`, `body`, `step`, `delay`, `variant`) rejected at import — they conflict with sequence YAML fields
- At send time, any remaining unresolved `{{variables}}` are stripped (not sent literally), double spaces collapsed, and a warning logged
- Emails with empty subject or body after rendering are marked `failed` and not sent
- Custom CSV columns stored as JSON in `leads.custom_fields`, parsed at send time
- Reimporting a lead updates its fields from the new CSV (source of truth), not silently skipped via INSERT OR IGNORE

## Account Providers

Accounts are provider-aware:

- `gws`: Google Workspace/Gmail account authenticated through gws OAuth. Uses Gmail API send/list/get operations.
- `smtp_imap`: generic email account. Uses SMTP for sending and IMAP for reply, unsubscribe, and bounce polling.

SMTP/IMAP accounts store only secret references, not raw passwords. The supported secret reference scheme today is `env:NAME`, resolved at runtime by `tick` and `account verify`. The CLI does not auto-discover `.env` files; operators can pass an explicit `--env-file <path>` to load `KEY=VALUE` pairs before any command resolves `env:NAME` references.

`cold-cli account verify <email>` is the operational check for SMTP/IMAP accounts. It resolves the configured secret references, authenticates to SMTP, authenticates to IMAP, and selects the inbox mailbox.

`cold-cli account update-smtp <email>` updates SMTP/IMAP provider settings without remove/re-add churn. Only provided flags change, and daily-limit updates still rebalance pending schedules.

## Provider Interfaces

```go
type GWSClient interface {
    SendEmail(account, to, rawMsg, threadID string) (msgID, threadID string, err error)
    ListMessages(account, query string) ([]GWSMessage, error)
    GetMessage(account, msgID string) (*GWSMessage, error)
}

type SMTPEmailSender interface {
    SendEmail(account Account, params EmailParams) (messageID string, threadID string, err error)
}

type IMAPMessageLister interface {
    ListMessages(account Account, since time.Time, includeSpamTrash bool) ([]GWSMessage, error)
}
```

- Real implementation calls gws as subprocess with 30s timeout
- Per-account config dirs for multi-account OAuth
- SMTP sends use implicit TLS (`ssl`), STARTTLS, or plaintext based on account config
- IMAP polling checks inbox plus spam/trash folders for bounce detection
- Per-send error isolation: provider failure marks that `scheduled_sends` row as `'failed'`, logs error, continues to next send
- Health check on `cold-cli init`: verify gws binary exists for Google Workspace accounts
- `last_poll_at` stored for efficient reply polling

## Error Handling

- **gws not found**: reported at `init`; only required for `gws` accounts
- **gws send failure**: per-send isolation, mark `'failed'`, slog.Error, continue
- **SMTP send failure**: per-send isolation, mark `'failed'`, slog.Error, continue
- **SMTP/IMAP credential failure**: `account verify` exits non-zero and reports which side failed
- **Missing message_id after step 1**: treated as send failure (prevents broken threading)
- **DB write failure after send**: slog.Error with full context (send_id, message_id), continue
- **Concurrent tick**: SQLite uses flock, Postgres uses advisory locks; second tick exits cleanly
- **Lock after crash**: SQLite flock and Postgres advisory locks both release on process exit / connection close
- **Invalid campaign update**: timezone, time format, send days validated before writing; successful send-window/day/timezone updates recalculate pending `scheduled_sends`

## Domain Diagnostics

`cold-cli doctor` checks sending domains for deliverability:
- **MX records** via DNS lookup
- **SPF** via TXT record lookup
- **DKIM** via TXT lookup across 19 common selectors (google, default, selector1/2, key1/2/3, etc.)
- **DMARC** via TXT lookup at `_dmarc.<domain>`
- **Domain age** via WHOIS lookup

Auto-detects domains from registered accounts if no domain specified.

## CLI Interface

```
cold-cli init
cold-cli doctor [domain...]

cold-cli --workspace <id> account add <email>
cold-cli --workspace <id> account add-smtp <email> --smtp-host ... --smtp-password-ref env:NAME --imap-host ...
cold-cli account update-smtp <email> [--smtp-host ...] [--imap-host ...] [--daily-limit N]
cold-cli account verify <email>
cold-cli --workspace <id> account list
cold-cli account list --all-workspaces
cold-cli account pause/resume/remove/update <email>

cold-cli campaign init [directory]
cold-cli --workspace <id> campaign create --name --sequence --leads --accounts [--start-date YYYY-MM-DD] [--send-days "1,2,3,4,5"]
cold-cli --workspace <id> campaign create --name --sequence-inline '...' --leads-inline '...' --accounts
cold-cli campaign clone <source> --name <new> --leads <csv>  # or --leads-inline
cold-cli campaign add-leads <name|id> --leads <csv>          # or --leads-inline
cold-cli campaign remove-lead <name|id> <email>
cold-cli campaign preview <name|id> [--render] [--lead <email>]
cold-cli campaign activate [--send-now] / pause/resume/status <name|id>
cold-cli campaign send-now <name|id>
cold-cli campaign update <name|id> [--sequence path] [--send-days "..."] [--send-window-start HH:MM] [--send-window-end HH:MM] [--timezone TZ] [--min-gap N] [--max-gap N]
cold-cli campaign list/delete/retry <name|id>

cold-cli tick [--dry-run] [--now]

cold-cli stats [campaign] [--leads] [--variants]
cold-cli log [campaign] [--limit N]

cold-cli lead list [--domain X] [--status X] [--limit N]
cold-cli lead pause/resume/blacklist <email|domain>
```

Workspace resolution for account/campaign commands is `--workspace`, then
`COLD_CLI_WORKSPACE_ID`, then `default`. Existing self-hosted installs keep
working because migrations backfill `accounts.workspace_id` and
`campaigns.workspace_id` to `default`. Campaign creation validates that all
selected sending accounts are active in the campaign workspace; `tick` remains
global and processes scheduled sends by concrete account/campaign IDs. Ownership
lives directly on `accounts` and `campaigns`, not in a separate web-app mapping.

All commands support `--json` for agent consumption. No interactive prompts, everything via flags.

## System Diagram

```
                    ┌─────────────────────────┐
                    │      cold-cli CLI        │
                    │      (Cobra)             │
                    ├─────────────────────────┤
                    │ init / doctor            │
                    │ account add/add-smtp/     │
                    │   verify/list/pause/      │
                    │   resume/remove/update    │
                    │ campaign init/create/    │
                    │   clone/                 │
                    │   add-leads/preview/     │
                    │   activate/pause/resume/ │
                    │   status/list/update/del/│
│   retry                  │
                    │ tick [--dry-run]         │
                    │ stats [--leads/variants] │
                    │ log [--limit]            │
                    │ lead list/pause/blacklist│
                    └────────┬────────────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
              ▼              ▼              ▼
     ┌────────────┐  ┌────────────┐  ┌───────────┐
     │ scheduler  │  │  tick      │  │  stats /  │
     │            │  │  engine    │  │  log      │
     │ • compute  │  │            │  │           │
     │   send_at  │  │ • lock     │  │ • agg by  │
     │ • round-   │  │ • poll     │  │   campaign│
     │   robin    │  │   replies  │  │   /step   │
     │   accounts │  │ • detect   │  │   /variant│
     │ • assign   │  │   unsubs   │  │ • event   │
     │   variants │  │ • poll     │  │   log     │
     │ • validate │  │   bounces  │  └───────────┘
     │   templates│  │ • send due │
     └─────┬──────┘  │ • slog    │
           │         └──┬───┬────┘
           ▼            │   │
     ┌────────────────────┐ │   ▼
     │  Storage backend   │◄┘  ┌──────────────┐
     │                    │    │ Provider     │
     │ • SQLite local DB  │    │ clients      │
     │ • Postgres shared  │    ├──────────────┤
     │   database         │    │ SendEmail()  │
     └────────────────────┘    │ ListMessages()│
                                │ Verify()     │
                                └──────┬───────┘
     ┌───────────┐                │
     │ tick.log  │                ▼
     │ (slog     │         ┌──────────────┐
     │  JSON)    │         │ gws CLI or   │
     └───────────┘         │ SMTP/IMAP    │
                           │ transports   │
     ┌───────────┐         └──────────────┘
     │  doctor   │
     │ • DNS MX  │
     │ • SPF/DKIM│
     │ • DMARC   │
     │ • WHOIS   │
     └───────────┘
```

## Design Decisions Log

| # | Decision | Choice | Rationale |
|---|---------|--------|-----------|
| 1 | Scheduling model | Eager (scheduled_sends table) | Enables campaign preview, agent review, simple tick |
| 2 | Concurrent tick protection | dialect-specific lock | SQLite uses flock, Postgres uses advisory lock on a dedicated connection |
| 3 | Thread management | Backfill thread_id onto scheduled_sends | Self-contained rows, no joins at send time |
| 4 | Daily limit tracking | COUNT from events table | No mutable counter, always accurate |
| 5 | Catch-up after sleep | Send all overdue with gaps | Daily limit + send window + send day are safety valves |
| 6 | Reply cancellation status | 'skipped' (vs 'cancelled') | Distinguishes auto-skip from user-initiated cancel |
| 7 | Project structure | Flat internal/ package | Right-sized for ~15 files, no over-nesting |
| 8 | CLI framework | Cobra | Industry standard, subcommand nesting |
| 9 | Template engine | strings.ReplaceAll | No injection risk, dead simple |
| 10 | gws error handling | Per-send isolation | Failure marks one send 'failed', continues to next |
| 11 | Account rotation | Round-robin at schedule time | Deterministic, previewable, thread continuity |
| 12 | Test strategy | GWSClient interface mock, real SQLite plus Postgres boundary coverage | Only external dep mocked, high-confidence tests while keeping Postgres seams covered |
| 13 | Template validation | At campaign creation | Catches missing fields before any sends |
| 14 | CSV schema | email required, rest driven by template | Flexible, no arbitrary constraints |
| 15 | Daily count query | Preload at tick start | One GROUP BY query, in-memory map |
| 16 | Reply polling | last_poll_at + after: filter | Efficient, only checks new messages |
| 27 | Reply matching | In-Reply-To primary, thread-ID fallback | Precise when possible, catches shared-inbox/forward replies as fallback |
| 17 | Sequence storage | YAML content in DB + file path fallback | Survives file moves, immutable per campaign |
| 18 | Daily limit timezone | Config default_timezone for day boundary | Prevents limit overshoot near midnight |
| 19 | Send day enforcement | Check day-of-week at tick time | Prevents weekend catch-up sends |
| 20 | Unsubscribe detection | Keyword matching on reply subject/snippet | Auto-blacklists globally, no manual intervention |
| 21 | Tick logging | log/slog JSON to ~/.cold-cli/tick.log | Works with cron, no redirection needed |
| 22 | List-Unsubscribe header | Opt-in (off by default) | Cold email should look like 1-to-1, not marketing |
| 23 | Domain diagnostics | DNS + WHOIS, no external APIs | Works offline, no rate limits on DNS |
| 24 | Campaign resolution | Accept name or numeric ID | Users instinctively use IDs from `campaign list` |
| 25 | Preview warnings | Run the same rebalance as preview/tick before warning | Warning output matches real sender-capacity schedule |
| 26 | Account re-add | Reactivate removed accounts on `add` | Remove shouldn't be a permanent one-way door |
| 28 | Account providers | Store provider-specific account config behind one account model | Keeps scheduling/account rotation shared while allowing gws and SMTP/IMAP transports |
| 29 | SMTP/IMAP secrets | Store secret references, not raw passwords | Keeps the database free of mailbox passwords and lets deployments provide secrets through env |
