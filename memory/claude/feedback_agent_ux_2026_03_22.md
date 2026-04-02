---
name: Agent UX feedback round 2
description: Agent feedback from 2026-03-22 session — lead resume, campaign remove-lead, inline creation deferred
type: feedback
---

Agent feedback addressed on 2026-03-22:
- Added `lead resume` (undo pause)
- Added `campaign remove-lead <campaign> <email>`
- Clarified daily-limit help text: "shared across all campaigns"
- `send-days` now accepts day names (mon,tue,...) not just numbers
- `campaign preview --render --lead <email>` for specific lead preview
- `campaign status` now shows send-days
- `stats` help clarified that it takes optional campaign name

**Deferred:** Inline sequence/leads creation (--sequence-inline, --leads-inline) was deprioritized — bigger design effort. If agents continue requesting this, revisit.

**Why:** The tool assumed you get it right the first time, but real usage is messy and iterative. Adjustments after creation (removing leads, unpausing) were the biggest friction.

**How to apply:** When adding new features, always consider the "undo" path. Every action that modifies state should have a reverse operation.
