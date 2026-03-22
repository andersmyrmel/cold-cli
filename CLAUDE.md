# cold-cli

Agent-first CLI cold email sequence engine in Go. See ARCHITECTURE.md for full design.

## Quick Reference

- **Language:** Go
- **CLI framework:** Cobra
- **Database:** SQLite via `modernc.org/sqlite` (pure Go, no CGO)
- **External dependency:** gws CLI (subprocess calls for Gmail API)
- **Config/data dir:** `~/.cold-cli/`

## Project Structure

```
cmd/cold-cli/main.go     — CLI entry, Cobra command definitions
internal/                 — single flat package, all application logic
  db.go                   — SQLite setup, schema migrations, indexes
  models.go               — structs (Account, Lead, Campaign, ScheduledSend, Event)
  tick.go                 — tick engine (flock, poll, send loop)
  scheduler.go            — eager schedule computation, variant assignment, round-robin
  gws.go                  — GWSClient interface + real subprocess implementation
  send.go                 — RFC 2822 message construction, threading headers
  reply.go                — reply/bounce detection, In-Reply-To header matching
  template.go             — {{placeholder}} string replacement via strings.ReplaceAll
  csv.go                  — lead CSV import, BOM stripping, field validation
  config.go               — YAML config loading
  campaign.go             — campaign CRUD, preview, rendered preview, daily limit warnings
  account.go              — account CRUD, update, domain diagnostics
  lead.go                 — lead pause/resume/blacklist/list, campaign remove-lead
  stats.go                — campaign/step/variant/lead stats, event log
```

## Key Design Decisions

These are settled — do not revisit without explicit instruction:

1. **Eager scheduling** — all sends pre-computed into `scheduled_sends` table at campaign creation. Do NOT use lazy/rolling `next_send_at` on campaign_leads.
2. **GWSClient interface** — gws interaction goes through an interface (`SendEmail`, `ListMessages`). Real impl calls subprocess. Tests use a mock.
3. **Template rendering** — `strings.ReplaceAll` for `{{placeholder}}` substitution. No Go `text/template`. No template engine.
4. **Daily limits** — count from events table (`SELECT COUNT(*) ... WHERE type='sent' AND timestamp >= today`). No mutable `sends_today` counter on accounts.
5. **Account rotation** — round-robin at schedule time. All steps for one lead use the same account (thread continuity).
6. **Thread management** — after step 1 send, backfill `thread_id` and `parent_message_id` onto all remaining `scheduled_sends` for that lead+campaign.
7. **Error isolation** — gws send failure marks that one `scheduled_sends` row as `'failed'` and continues. Never crash the whole tick.
8. **Status semantics** — `skipped` = auto-cancelled (reply/bounce/domain-reply). `cancelled` = user action (pause/blacklist). These are distinct.
9. **File lock** — tick uses flock/fcntl on `~/.cold-cli/tick.lock`. OS auto-releases on process exit.
10. **Validation at creation** — template placeholders validated against lead CSV at campaign creation, not send time.

## Testing

- Use real SQLite (`:memory:`) in tests. Do NOT mock the database.
- Mock only the `GWSClient` interface.
- Test scheduler, template rendering, reply matching, bounce parsing as pure functions.
- Every codepath needs: happy path + key error branches.

## Build & Run

```bash
go build -o cold-cli ./cmd/cold-cli
go test ./...
```

## scheduled_sends Status Values

```
pending   → waiting to send
sent      → successfully sent via gws
failed    → gws send failed (logged, isolated)
skipped   → auto-cancelled (reply/bounce/domain-reply detected)
cancelled → user-cancelled (pause/blacklist)
```

## Data Model

6 tables: `accounts`, `campaigns`, `campaign_accounts`, `leads`, `campaign_leads`, `scheduled_sends`, `events`. See ARCHITECTURE.md for full schema.

Key: `scheduled_sends` is the core table. Each row is a self-contained send instruction with pre-computed `send_at`, assigned `account_id`, `variant_index`, and (after step 1 sends) `thread_id` + `parent_message_id`.

## CSV Import Rules

- `email` column is always required
- All other required columns are driven by `{{placeholders}}` in the sequence YAML
- Strip UTF-8 BOM on import
- Validate all leads have values for all placeholders at campaign creation

## gws Integration

- Always call via subprocess with 30s timeout
- Parse stdout for message_id/thread_id after send
- Capture stderr on failure for error reporting
- Health check on `cold-cli init`: verify gws binary exists and can auth
- Reply polling: use `last_poll_at` timestamp + `after:` query filter
