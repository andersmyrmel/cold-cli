# cold-cli Architecture

Open-source, agent-first CLI cold email sequence engine in Go. Built on top of [gws CLI](https://github.com/nicholasgasior/gws) for Gmail API operations.

## Problem

No CLI-native cold email sequence engine exists. SaaS tools (Instantly, Smartlead, Lemlist) are all GUIs. We want the sequence engine layer only, gws handles Gmail send/receive.

## Tech Stack

- **Go** single binary, no runtime deps
- **SQLite** single file (`~/.cold-cli/data.db`), pure Go driver (`modernc.org/sqlite`, no CGO)
- **gws CLI** subprocess calls for Gmail send + inbox polling
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
│   ├─ db.go                (SQLite setup, migrations, indexes)
│   ├─ models.go            (structs: Account, Lead, Campaign, etc.)
│   ├─ tick.go              (tick engine: lock, poll, send loop)
│   ├─ scheduler.go         (eager schedule computation, variant assignment)
│   ├─ gws.go               (GWSClient interface + real subprocess impl)
│   ├─ send.go              (email construction: RFC 2822, threading, List-Unsubscribe)
│   ├─ reply.go             (reply/bounce/unsubscribe detection, header matching)
│   ├─ template.go          ({{placeholder}} string replacement)
│   ├─ csv.go               (lead CSV import, BOM stripping, validation)
│   ├─ config.go            (YAML config loading)
│   ├─ stats.go             (campaign/step/variant/lead stats, event log)
│   ├─ account.go           (account CRUD, domain diagnostics)
│   ├─ lead.go              (lead pause/resume/blacklist/list, campaign remove-lead)
│   └─ campaign.go          (campaign CRUD, clone, add-leads, inline creation)
├─ go.mod
└─ go.sum
```

## Data Model

```
accounts
├─ id
├─ email
├─ daily_limit
├─ last_send_at
├─ status (active/paused/removed)
└─ gws_config_dir

campaigns
├─ id, name, status, sequence_file, sequence_content
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
├─ global_status (active/blacklisted/bounced)
└─ created_at

campaign_leads
├─ campaign_id, lead_id
├─ status (active/completed/replied/bounced/paused)
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
└─ sent_at                    (filled after send)

events
├─ id, campaign_id, lead_id, account_id
├─ type (sent/reply/bounce/unsubscribe)
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
├─ acquire ~/.cold-cli/tick.lock (flock/fcntl)
│   └─ locked? → print "tick already running", exit 0
│
├─ 1. Poll inbox (via gws, messages after last_poll_at)
│      → match replies via In-Reply-To header
│      → detect unsubscribe requests (keyword matching)
│      │   → unsubscribe: blacklist lead globally, cancel all sends
│      │   → reply: UPDATE campaign_leads.status = 'replied',
│      │            skip remaining sends
│      → if stop_on_domain_reply: skip same-domain leads in campaign
│      → structured JSON logging via log/slog for all operations
│
├─ 2. Poll inbox for bounce NDRs (MAILER-DAEMON)
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
├─ 4. SELECT * FROM scheduled_sends
│     WHERE send_at <= now AND status = 'pending'
│     (filter: account under daily limit, within send window + send day)
│
├─ 5. For each send:
│      ├─ load sequence from DB content (fallback to file for pre-migration)
│      ├─ load lead fields including custom_fields JSON
│      ├─ render template (strings.ReplaceAll)
│      ├─ construct RFC 2822 message
│      │   step 1: new thread (Subject, From, To)
│      │   step 2+: In-Reply-To, References, Re: Subject, thread_id
│      │   optional: List-Unsubscribe header (if configured)
│      ├─ call gws send (30s timeout)
│      │   success → mark 'sent', INSERT event (error-checked + logged)
│      │   failure → mark 'failed', slog.Error, continue
│      ├─ validate message_id/thread_id returned (else mark failed)
│      ├─ if step 1: backfill thread_id + parent_message_id
│      │   onto all future scheduled_sends for this lead+campaign
│      ├─ increment in-memory daily count
│      └─ sleep 90-140 sec (random)
│
├─ 6. Log summary to tick.log, print human-readable summary
│
└─ release lock
```

## Eager Scheduling

All send times pre-computed at campaign creation. Each send = a `scheduled_sends` row.

Enables:
- `campaign preview` to show full schedule before activating
- Agent review of the timeline
- tick is trivially simple: `SELECT WHERE send_at <= now AND status = 'pending'`

### Schedule Computation

At `campaign create` (or `clone` / `add-leads`):
1. Parse sequence YAML (steps, delays, variants)
2. Parse leads CSV (validate `email` + all `{{placeholders}}` used in sequence)
3. Assign accounts round-robin (all steps for one lead = same account for thread continuity)
4. Assign variants (round-robin across leads for each step that has variants)
5. Compute `send_at` for each lead+step:
   - Step 1: campaign start time + offset based on lead position
   - Step N: previous step's send_at + delay days
   - Clamp to send window (start/end hours)
   - Skip non-send days (e.g., weekends)
   - Add jitter within min/max gap range
6. INSERT all `scheduled_sends` rows with status='pending'

### Catch-Up After Laptop Sleep

tick processes all overdue sends (`send_at <= now`) with normal 90-140 sec gaps. Daily limit, send window, and send day are the safety valves. No staleness cutoff. Recipients don't see the originally scheduled time.

## Account Rotation

Round-robin assignment at schedule time (not send time). All steps for a given lead use the same account (required for Gmail thread continuity). Assignment is deterministic and visible in `campaign preview`.

## Reply/Bounce/Unsubscribe Handling

- **Reply detected** → `campaign_leads.status = 'replied'`, remaining `scheduled_sends` marked `'skipped'`
- **Domain reply** (if `stop_on_domain_reply=true`) → all leads with same domain in that campaign get their pending sends skipped
- **Unsubscribe detected** (keyword matching: "unsubscribe", "remove me", "opt out", etc.) → lead blacklisted globally, all pending sends across all campaigns cancelled, `'unsubscribe'` event recorded
- **Bounce detected** → `leads.global_status = 'bounced'` (global), `campaign_leads.status = 'bounced'`, pending sends skipped
- Daily send counts derived from `COUNT(*) FROM events WHERE type='sent'` with timezone-aware day boundary

## Template Engine

Simple `strings.ReplaceAll` for `{{placeholder}}` substitution. No template engine, no injection risk.

- Placeholders validated at campaign creation: extract all `{{X}}` from sequence YAML, verify every lead has non-empty values
- CSV schema: `email` is the only hardcoded required column; all other required columns are driven by the sequence's placeholders
- Custom CSV columns stored as JSON in `leads.custom_fields`, parsed at send time

## gws Integration

```go
type GWSClient interface {
    SendEmail(account, to, rawMsg string) (msgID, threadID string, err error)
    ListMessages(account, query string) ([]Message, error)
}
```

- Real implementation calls gws as subprocess with 30s timeout
- Per-account config dirs for multi-account OAuth
- Per-send error isolation: gws failure marks that `scheduled_sends` row as `'failed'`, logs error, continues to next send
- Health check on `cold-cli init`: verify gws binary exists
- `last_poll_at` stored for efficient reply polling (`after:` query filter)

## Error Handling

- **gws not found**: caught at `init` and first `tick` run
- **gws send failure**: per-send isolation, mark `'failed'`, slog.Error, continue
- **Missing message_id after step 1**: treated as send failure (prevents broken threading)
- **DB write failure after send**: slog.Error with full context (send_id, message_id), continue
- **Concurrent tick**: flock auto-releases on process exit; second tick exits cleanly
- **Lock file after crash**: OS releases flock on process exit, no stale lock problem
- **Invalid campaign update**: timezone, time format, send days validated before writing

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

cold-cli account add/list/pause/resume/remove/update

cold-cli campaign init [directory]
cold-cli campaign create --name --sequence --leads --accounts [--start-date YYYY-MM-DD]
cold-cli campaign create --name --sequence-inline '...' --leads-inline '...' --accounts
cold-cli campaign clone <source> --name <new> --leads <csv>  # or --leads-inline
cold-cli campaign add-leads <name|id> --leads <csv>          # or --leads-inline
cold-cli campaign remove-lead <name|id> <email>
cold-cli campaign preview <name|id> [--render] [--lead <email>]
cold-cli campaign activate/pause/resume/status <name|id>
cold-cli campaign list/update/delete/retry <name|id>

cold-cli tick [--dry-run]

cold-cli stats [campaign] [--leads] [--variants]
cold-cli log [campaign] [--limit N]

cold-cli lead list [--domain X] [--status X]
cold-cli lead pause/resume/blacklist <email|domain>
```

All commands support `--json` for agent consumption. No interactive prompts, everything via flags.

## System Diagram

```
                    ┌─────────────────────────┐
                    │      cold-cli CLI        │
                    │      (Cobra)             │
                    ├─────────────────────────┤
                    │ init / doctor            │
                    │ account add/list/pause/  │
                    │   resume/remove/update   │
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
     │   send_at  │  │ • flock    │  │ • agg by  │
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
     ┌───────────┐      │   ▼
     │  SQLite   │◄─────┘  ┌──────────────┐
     │  (~/.cold-│         │  GWSClient   │
     │  cli/     │         │  (interface)  │
     │  data.db) │         ├──────────────┤
     │           │         │ SendEmail()  │
     └───────────┘         │ ListMessages()│
                           └──────┬───────┘
     ┌───────────┐                │
     │ tick.log  │                ▼
     │ (slog     │         ┌──────────────┐
     │  JSON)    │         │   gws CLI    │
     └───────────┘         │  (subprocess)│
                           │  Gmail API   │
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
| 2 | Concurrent tick protection | flock file lock | OS auto-releases on exit, standard cron pattern |
| 3 | Thread management | Backfill thread_id onto scheduled_sends | Self-contained rows, no joins at send time |
| 4 | Daily limit tracking | COUNT from events table | No mutable counter, always accurate |
| 5 | Catch-up after sleep | Send all overdue with gaps | Daily limit + send window + send day are safety valves |
| 6 | Reply cancellation status | 'skipped' (vs 'cancelled') | Distinguishes auto-skip from user-initiated cancel |
| 7 | Project structure | Flat internal/ package | Right-sized for ~15 files, no over-nesting |
| 8 | CLI framework | Cobra | Industry standard, subcommand nesting |
| 9 | Template engine | strings.ReplaceAll | No injection risk, dead simple |
| 10 | gws error handling | Per-send isolation | Failure marks one send 'failed', continues to next |
| 11 | Account rotation | Round-robin at schedule time | Deterministic, previewable, thread continuity |
| 12 | Test strategy | GWSClient interface mock, real SQLite | Only external dep mocked, high-confidence tests |
| 13 | Template validation | At campaign creation | Catches missing fields before any sends |
| 14 | CSV schema | email required, rest driven by template | Flexible, no arbitrary constraints |
| 15 | Daily count query | Preload at tick start | One GROUP BY query, in-memory map |
| 16 | Reply polling | last_poll_at + after: filter | Efficient, only checks new messages |
| 17 | Sequence storage | YAML content in DB + file path fallback | Survives file moves, immutable per campaign |
| 18 | Daily limit timezone | Config default_timezone for day boundary | Prevents limit overshoot near midnight |
| 19 | Send day enforcement | Check day-of-week at tick time | Prevents weekend catch-up sends |
| 20 | Unsubscribe detection | Keyword matching on reply subject/snippet | Auto-blacklists globally, no manual intervention |
| 21 | Tick logging | log/slog JSON to ~/.cold-cli/tick.log | Works with cron, no redirection needed |
| 22 | List-Unsubscribe header | Opt-in (off by default) | Cold email should look like 1-to-1, not marketing |
| 23 | Domain diagnostics | DNS + WHOIS, no external APIs | Works offline, no rate limits on DNS |
| 24 | Campaign resolution | Accept name or numeric ID | Users instinctively use IDs from `campaign list` |
| 25 | Preview warnings | Show daily limit overflow inline | Only warn when sends exceed limit, no noise otherwise |
| 26 | Account re-add | Reactivate removed accounts on `add` | Remove shouldn't be a permanent one-way door |
