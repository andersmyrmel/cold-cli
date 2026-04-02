---
name: Agent UX feedback from two cold-cli users
description: Feedback from two AI agents using cold-cli in real repos — common pain points and feature requests
type: feedback
---

Two agents independently tested cold-cli (2026-03-16). Common themes and feature requests:

## Both agents hit these:
1. **campaign status takes name not ID** — both tried `campaign status 5` and got errors. campaign list shows IDs prominently so users reach for them first. Fix: accept both name and ID.
2. **No rendered preview** — preview shows schedule but not actual email content with variables filled in. Want `--render` flag or similar to see first lead's actual email.
3. **Preview doesn't reflect send-day/window restrictions** — shows theoretical earliest dates with "enforced at send time" note, but users want to see actual send dates.

## Agent 1:
4. No `--start-date` flag on create or activate
5. `campaign list` should show send window and send-days
6. Unclear what happens on reply (docs gap)

## Agent 2:
7. No example YAML/CSV in help — wants `campaign init` scaffolding command
8. No way to update campaign copy in place (`campaign update --sequence`)
9. No `content_type` field in sequence YAML (HTML vs plain text)
10. `--skip-auth` flag naming is confusing
11. `doctor` doesn't auto-check newly added accounts

**Why:** These are real users hitting real friction. The name-vs-ID issue and preview gaps were the biggest blockers.
**How to apply:** Prioritize the items both agents flagged. Use this list when planning cold-cli improvements.
