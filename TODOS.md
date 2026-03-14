# cold-cli Implementation Plan

Ordered phases. Each phase builds on the previous. A phase is complete when all items are checked and tests pass.

See ARCHITECTURE.md for full design. See CLAUDE.md for coding conventions.

---

## Phase 1: Scaffold + Database

Bootstrap the Go project and get the data layer working.

- [x] `go mod init`, add dependencies: cobra, modernc.org/sqlite, gopkg.in/yaml.v3
- [x] `cmd/cold-cli/main.go` — Cobra root command with `--json` global flag
- [x] `internal/db.go` — SQLite setup (`~/.cold-cli/data.db`), schema migration with all 7 tables + indexes (see ARCHITECTURE.md)
- [x] `internal/models.go` — Go structs for all tables
- [x] `internal/config.go` — YAML config loading (`~/.cold-cli/config.yml`)
- [x] `cold-cli init` command — create `~/.cold-cli/` dir, database, config template, gws health check (verify binary exists + can auth)
- [x] Tests: DB creation, schema correctness, idempotent re-init, gws check failure handling

## Phase 2: Accounts + Leads

Basic entity management before we can create campaigns.

- [x] `cold-cli account add <email>` — validate email format, check gws auth for account, INSERT
- [x] `cold-cli account list` — list accounts with status, support `--json`
- [x] `internal/csv.go` — CSV parser with UTF-8 BOM stripping, header normalization
- [x] `internal/template.go` — extract `{{placeholders}}` from strings, `strings.ReplaceAll` rendering
- [x] `cold-cli lead pause <email>` — set campaign_leads.status = 'paused', cancel pending scheduled_sends
- [x] `cold-cli lead blacklist <email|domain>` — set leads.global_status = 'blacklisted', cancel pending sends across all campaigns
- [x] Tests: duplicate account rejection, email validation, CSV parsing (BOM, missing columns, empty values), template placeholder extraction, template rendering, blacklist by email vs domain

## Phase 3: Campaign Create + Scheduler

The eager scheduling engine — the core differentiator.

- [x] `internal/scheduler.go` — sequence YAML parsing, schedule computation:
  - Round-robin account assignment (all steps for one lead → same account)
  - A/B variant assignment (`variant_index` on scheduled_sends)
  - `send_at` computation: step delays, send window clamping, send day skipping (weekends), jitter
- [x] Template validation at creation: extract all `{{placeholders}}` from sequence, verify every lead has values
- [x] CSV schema validation: `email` always required, other required columns driven by sequence placeholders
- [x] `cold-cli campaign create --name --sequence --leads --accounts` — parse YAML + CSV, validate, compute schedule, INSERT campaign + campaign_leads + campaign_accounts + scheduled_sends, status = 'draft'
- [x] `cold-cli campaign preview <name>` — SELECT scheduled_sends ORDER BY send_at, format as table or `--json`
- [x] `cold-cli campaign activate <name>` — UPDATE status → 'active' (only from 'draft')
- [x] `cold-cli campaign pause <name>` — UPDATE status → 'paused'
- [x] `cold-cli campaign resume <name>` — UPDATE status → 'active' (only from 'paused')
- [x] `cold-cli campaign status <name>` — show campaign details + counts by scheduled_send status
- [x] Tests: schedule computation (verify send_at values respect windows/days/jitter/delays), round-robin assignment, variant assignment, template validation (missing fields error), YAML parse errors, CSV parse errors, campaign state transitions (draft→active, active→paused, paused→active, reject invalid transitions)

## Phase 4: gws Integration Layer

The subprocess wrapper — abstracted behind an interface for testability.

- [ ] `internal/gws.go` — `GWSClient` interface: `SendEmail(account, to, rawMsg string) (msgID, threadID string, err error)` and `ListMessages(account, query string) ([]Message, error)`
- [ ] Real implementation: subprocess calls with 30s timeout, stdout parsing for message_id/thread_id, stderr capture on failure
- [ ] Mock implementation for tests: records calls, returns canned responses
- [ ] `internal/send.go` — RFC 2822 message construction:
  - Step 1: new thread (Subject, From, To, body)
  - Step 2+: reply in thread (Re: Subject, In-Reply-To, References, thread_id)
  - Template rendering (strings.ReplaceAll with lead fields)
  - Variant selection (pick subject/body based on variant_index)
- [ ] Validate message_id/thread_id returned after send — treat missing as failure
- [ ] Tests: message construction (step 1 vs step 2+ headers), variant selection, mock gws send/failure, message_id validation

## Phase 5: Tick Engine

The core runtime — poll, detect, send.

- [ ] `internal/reply.go` — reply detection:
  - Parse In-Reply-To header from inbox messages
  - Match against events.message_id
  - UPDATE campaign_leads.status = 'replied'
  - UPDATE scheduled_sends.status = 'skipped' (remaining sends for that lead+campaign)
  - If stop_on_domain_reply: find same-domain leads, skip their pending sends
- [ ] `internal/reply.go` — bounce detection:
  - Identify NDRs (from MAILER-DAEMON, postmaster, etc.)
  - Extract bounced email address
  - UPDATE leads.global_status = 'bounced'
  - UPDATE campaign_leads.status = 'bounced'
  - UPDATE scheduled_sends.status = 'skipped'
- [ ] `internal/tick.go` — tick engine:
  - Acquire flock on `~/.cold-cli/tick.lock` (exit 0 if locked)
  - Poll replies (gws ListMessages with `after:last_poll_at` filter)
  - Poll bounces
  - Preload daily send counts per account (one GROUP BY query)
  - SELECT scheduled_sends WHERE send_at <= now AND status = 'pending'
  - Filter: account under daily limit, current time within send window
  - For each send: render → construct message → gws send → handle result
    - Success: mark 'sent', INSERT event, increment in-memory count
    - Failure: mark 'failed', log error, continue to next
    - Step 1 success: backfill thread_id + parent_message_id onto future sends
  - Sleep 90-140 sec random gap between sends
  - Update last_poll_at
  - Print summary (sent/failed/skipped counts)
- [ ] `cold-cli tick` command (calls tick engine)
- [ ] `cold-cli tick --dry-run` — run everything except actual gws sends, print what would happen
- [ ] Tests: full tick cycle (mock gws, real SQLite), reply detection (header matching, lead status update, sends cancelled), domain reply cascade, bounce detection (NDR parsing), daily limit enforcement (hit limit mid-tick), send window enforcement, lock contention (second tick exits), step 1 backfill, failed send isolation, dry-run output

## Phase 6: Stats + Polish

Reporting and final CLI polish.

- [ ] `cold-cli stats [campaign]` — aggregate events by campaign/step/type (sent/replied/bounced), support `--json`
- [ ] `cold-cli stats <name> --leads` — per-lead breakdown (status, steps completed, reply date)
- [ ] `--json` flag working on all commands
- [ ] Error messages: consistent format, actionable guidance (e.g., "run cold-cli init first", "account not found, run cold-cli account list")
- [ ] Tests: stats aggregation accuracy, empty state handling, JSON output format validation

---

## Post-v1 (not blocking)

- [ ] Full CSV encoding detection (Latin-1/Windows-1252) beyond BOM stripping
- [ ] Campaign delete (with cascade to scheduled_sends, campaign_leads, events)
- [ ] Retry failed sends (`cold-cli campaign retry-failed <name>`)
- [ ] Export campaign results to CSV
- [ ] Shell completions (Cobra built-in)
