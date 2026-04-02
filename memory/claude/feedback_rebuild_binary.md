---
name: Use go install, not go build
description: cold-cli is installed system-wide via go install; use "go install ./cmd/cold-cli" to rebuild, not "go build"
type: feedback
---

After making code changes, use `go install ./cmd/cold-cli` to update the binary — not `go build`.

**Why:** The user runs cold-cli from `~/go/bin/cold-cli` (on their PATH via `go install`). Running `go build -o cold-cli ./cmd/cold-cli` only creates a local binary in the repo root that nothing uses. The correct term is "reinstall the CLI" (not "rebuild the binary").

**How to apply:** After any code changes that affect the binary, run `go install ./cmd/cold-cli` to update the version the user actually runs.
