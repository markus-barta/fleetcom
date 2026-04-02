Assume the developer role for **FleetCom** — fleet management & agent monitoring platform.

Read `CLAUDE.md` for the full project context. Then wait for instructions — do NOT start any task until explicitly told.

## Quick Reference

### Architecture
- **Single binary**: Go backend serves API (`/api/*`) + static HTML on configurable port
- **SQLite**: WAL mode, FK enforced, `modernc.org/sqlite` (pure Go, no CGO)
- **Auth**: session cookies (HttpOnly, SameSite=Lax) + per-host bearer tokens for agents
- **Frontend**: Single HTML file, vanilla JS, no build step

### Key Conventions
- IDs: `strconv.ParseInt`, return 400 on failure
- Lists: always `[]T{}` not `nil` (JSON `[]` not `null`)
- Response helpers: `jsonOK(w, v)`, `jsonError(w, msg, code)`

### PPM (Task Tracking)
- Project: DSC26 (project ID: 2), Epic: DSC26-52
- Before starting work: check for backing ticket, start timer
- When done: update ticket status, stop timer
- API: `curl -s -H "Authorization: Bearer $PPMAPIKEY" https://pm.barta.cm/api/...`

### Critical Safety Rules
- **NEVER** read, cat, print, or source secret/env files (`~/Secrets/*`, `.env*`, `.age`, `/run/secrets/*`). Just use the env var — direnv loads it.
- **NEVER** run commands that could print secrets to stdout/stderr
- **NEVER** `docker compose down -v` or `docker volume rm` (destroys DB)
- **NEVER** edit applied migrations
- **ALWAYS** `go vet` before committing
- **ALWAYS** scan `git diff` for secrets before committing

---

Now confirm you have loaded the context and wait for the user's instruction.
