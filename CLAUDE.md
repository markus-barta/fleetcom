# CLAUDE.md — FleetCom

## Project

**FleetCom** — Fleet management & agent monitoring platform.
Central hub for managing DSC-AI agent fleet and NixOS infrastructure. Replaces NixFleet (legacy).

- **Domain**: fleet.barta.cm (+ redirect fleetcom.barta.cm)
- **PPM Project**: DSC Infrastructure (project ID: 2, key: DSC26)
- **PPM Epic**: DSC26-52 (FleetCom)

## Project Management: PPM is the Single Source of Truth

All task tracking, planning, and status updates happen in **PPM** at `https://pm.barta.cm`.

### Rules

- **Before starting work**: Check PPM for a backing ticket. If none exists, create one first.
- **Planning**: Create epics and tickets in PPM. Do not create local backlog files.
- **Status updates**: Update ticket status in PPM as work progresses (new → in-progress → done).
- **Time tracking**: Start timer when beginning work, stop when done. mba user_id=2.
- **Done means done**: Mark tickets as `done` in PPM only when acceptance criteria are met.

### PPM API Access

Auth: Bearer token via `$PPMAPIKEY` environment variable.
All requests: `curl -s -H "Authorization: Bearer $PPMAPIKEY" https://pm.barta.cm/api/...`

#### Core Endpoints

| Action | Method | Endpoint |
|--------|--------|----------|
| List project issues | GET | `/api/projects/2/issues` |
| Issue tree (hierarchy) | GET | `/api/projects/2/issues/tree` |
| Create issue in project | POST | `/api/projects/2/issues` |
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

- **Backend**: Go (stdlib net/http + chi router + SQLite via modernc.org/sqlite)
- **Frontend**: Single HTML file, no framework, vanilla JS
- **Database**: SQLite (WAL mode, pure Go driver)
- **Auth**: Password + TOTP, session cookies
- **Deployment**: Docker on csb1, behind Cloudflare DNS
- **Agent**: Shell script (cron) or lightweight Go binary on each host

## Architecture

```
Hosts (dsc0, csb0, csb1, hsb0, ...)
  └── fleetcom-agent (cron, HTTPS POST heartbeat)
        │
        ▼
FleetCom Server (csb1, Docker)
  ├── POST /api/heartbeat (bearer token auth)
  ├── GET /api/hosts (session cookie auth)
  ├── GET / (static HTML dashboard)
  └── SQLite (hosts, containers, agents, sessions)
        │
        ▼
Browser (fleet.barta.cm)
```

## Security

- All communication over HTTPS (Cloudflare edge TLS)
- Agent → server: per-host bearer token
- Browser → server: password + TOTP, HttpOnly session cookies
- Host communication over Tailscale mesh where possible
- No secrets stored in database
- No external dependencies for auth

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
