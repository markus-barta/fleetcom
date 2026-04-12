# CLAUDE.md — FleetCom

## Project

**FleetCom** — Fleet management & agent monitoring platform.
Central hub for managing DSC-AI agent fleet and NixOS infrastructure. Replaces NixFleet (legacy).

- **Domain**: fleet.barta.cm (+ redirect fleetcom.barta.cm)
- **PPM Project**: FleetCom (project ID: 4, key: FLEET)
- **PPM Epic**: FLEET-1 (FleetCom MVP)

## Project Management: PPM is the Single Source of Truth

All task tracking, planning, and status updates happen in **PPM** at `https://pm.barta.cm`.

### Rules

- **Before starting work**: Check PPM for a backing ticket. If none exists, create one first.
- **Planning**: Create epics and tickets in PPM. Do not create local backlog files.
- **Status updates**: Update ticket status in PPM as work progresses (new → in-progress → done).
- **Time tracking**: Start timer when beginning work, stop when done. mba user_id=2.
- **Done means done**: Mark tickets as `done` in PPM only when acceptance criteria are met.

### PPM API Access

Auth: Bearer token via `$PPMAPIKEY` environment variable (loaded from `~/Secrets/ppm.env` via direnv).
All requests: `curl -s -H "Authorization: Bearer $PPMAPIKEY" https://pm.barta.cm/api/...`

#### Core Endpoints

| Action | Method | Endpoint |
|--------|--------|----------|
| List project issues | GET | `/api/projects/4/issues` |
| Issue tree (hierarchy) | GET | `/api/projects/4/issues/tree` |
| Create issue in project | POST | `/api/projects/4/issues` |
| Single issue | GET | `/api/issues/{id}` |
| Update issue | PUT | `/api/issues/{id}` |
| Delete issue | DELETE | `/api/issues/{id}` |
| Issue children | GET | `/api/issues/{id}/children` |
| Issue comments | GET | `/api/issues/{id}/comments` |
| Add comment | POST | `/api/issues/{id}/comments` body: `{body}` |
| Search | GET | `/api/search?q=...` |
| Time entries | GET | `/api/issues/{id}/time-entries` |
| Create time entry | POST | `/api/issues/{id}/time-entries` body: `{"user_id": 2}` or `{"user_id": 2, "override": 1.5, "comment": "..."}` |
| Update time entry | PUT | `/api/time-entries/{id}` body: `{"stopped_at": "ISO8601"}` |

#### Filtering (query params, comma-separated)

`?status=new,in-progress` `?priority=high` `?type=epic,ticket` `?limit=50&offset=0`

#### Create/Update Issue Body

```json
{"title":"...","type":"ticket","status":"new","priority":"medium","parent_id":172,"description":"...","acceptance_criteria":"..."}
```

### Valid PPM Values

- **Types**: `epic`, `ticket`, `task`
- **Statuses**: `new`, `backlog`, `in-progress`, `qa`, `done`, `accepted`, `invoiced`, `cancelled`
- **Priorities**: `low`, `medium`, `high`

## Tech Stack

- **Backend**: Go (stdlib net/http + chi router + SQLite via modernc.org/sqlite, pure Go, no CGO)
- **Real-time**: Server-Sent Events (SSE) — instant push to browser on heartbeat arrival
- **Frontend**: Single HTML file, Alpine.js + Lucide icons (self-hosted), no build step, no npm
- **Database**: SQLite (WAL mode, pure Go driver)
- **Auth**: Password + TOTP, HttpOnly session cookies (SameSite=Lax, Secure), per-host bearer tokens
- **Deployment**: Docker on csb1, behind Cloudflare DNS (edge TLS)
- **Bosun**: Go daemon in Docker container — Docker socket event stream + periodic heartbeat

## Architecture

```
Hosts (dsc0, csb0, csb1, hsb0, ...)
  └── fleetcom-bosun (Docker container, mounts host /var/run/docker.sock)
        ├── Docker event stream (real-time: die, restart, oom, health_status)
        │     → POST /api/container-events (immediate)
        └── Periodic heartbeat (60s, enriched: health, restart_count, uptime)
              → POST /api/heartbeat
        │
        ▼
FleetCom Server (csb1, Docker)
  ├── POST /api/heartbeat         (bearer token auth, agents push here)
  ├── POST /api/container-events  (real-time container lifecycle events)
  ├── GET  /api/events            (SSE stream, pushes updates to browser)
  ├── POST /login                 (password + TOTP → session cookie)
  ├── GET  /                      (static HTML + Alpine.js dashboard)
  └── SQLite (hosts, containers, agents, sessions, tokens, container_events)
        │
        ▼
Browser (fleet.barta.cm, Alpine.js + SSE — reactive, no polling)
```

### Bosun Deployment

```yaml
# On each host — agent/docker-compose.yml
fleetcom-bosun:
  image: ghcr.io/markus-barta/fleetcom-bosun:latest
  restart: unless-stopped
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock:ro
    - /proc:/host/proc:ro
    - /sys:/host/sys:ro
    - /etc/os-release:/host/etc/os-release:ro
  environment:
    - FLEETCOM_URL=https://fleet.barta.cm
    - FLEETCOM_TOKEN=${HOST_TOKEN}
    - FLEETCOM_HOSTNAME=${HOSTNAME}
    - FLEETCOM_AGENTS=${AGENTS_JSON}
```

## Secret Safety

**NEVER** read, cat, print, head, tail, echo, or source secret files to stdout. This includes:
- `~/Secrets/*`, `.env`, `.env.local`, `.age`, `.gpg`, `/run/secrets/*`, `/run/agenix/*`
- Any command where secret values could appear in stdout/stderr
- **Just use the env var** (e.g., `$PPMAPIKEY`) — direnv loads it automatically

If you need to verify a secret exists: check file existence (`ls -la`) or test the variable (`test -n "$PPMAPIKEY"`). **Never print the value.**

## Security

- All communication over HTTPS (Cloudflare edge TLS)
- Bosun → server: per-host bearer token
- Browser → server: password + TOTP, HttpOnly session cookies
- Host communication over Tailscale mesh where possible
- No secrets stored in database
- No external dependencies for auth

## Dependencies

External dependencies are acceptable when they provide clear value — evaluate each on merit rather than defaulting to "no deps." Self-host JS libraries (Alpine.js, Lucide) to avoid CDN reliance. No npm/build step required; keep the single-HTML-file architecture.

## Build & Run

```bash
# Backend dev
cd backend && go run .

# Build
go build -o fleetcom-server ./cmd/server

# Docker
docker compose build && docker compose up -d
```

## Reference

- NixFleet (predecessor): ~/Code/nixfleet — patterns to adopt, not code to fork
- DSC infra: ~/Code/dsccfg
- NixCfg infra: ~/Code/nixcfg
- PPM tool: ~/Code/ppm
