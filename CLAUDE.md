# CLAUDE.md — FleetCom

**Read `docs/DEPLOYMENT.md` at every session start.** Every deploy must bump the patch version in `.github/workflows/ci.yml` — never deploy the same version twice.

## Project

**FleetCom** — Fleet management & agent monitoring platform.
Central hub for managing DSC-AI agent fleet and NixOS infrastructure. Replaces NixFleet (legacy).

- **Domains**: fleet.barta.cm (personal), fleet.bytepoets.com (BYTEPOETS)
- **PPM Project**: FleetCom (project ID: 4, key: FLEET)
- **PPM Epic**: FLEET-1 (FleetCom MVP)
- **Infra repo**: BYTEPOETS/infracore (BP server docker-compose + nginx)

## Project Management: PPM is the Single Source of Truth

All task tracking, planning, and status updates happen in **PPM** at `https://pm.barta.cm`.

### Rules

- **Before starting work**: Check PPM for a backing ticket. If none exists, create one first.
- **Planning**: Create epics and tickets in PPM. Do not create local backlog files.
- **Status updates**: Update ticket status in PPM as work progresses (new → in-progress → done).
- **Time tracking**: Start timer when beginning work, stop when done. mba user_id=2.
- **Done means done**: Mark tickets as `done` in PPM only when acceptance criteria are met.

### PPM API Access

Auth: Bearer token via `$PPMAPIKEY` environment variable (loaded from `~/Secrets/PPMAPIKEY.env`).
Source it first: `source ~/Secrets/PPMAPIKEY.env` — then use `$PPMAPIKEY` and `$URL`.
All requests: `curl -s -H "Authorization: Bearer $PPMAPIKEY" https://$URL/api/...`

**NEVER** read, cat, or print the secrets file. Source it and use the variables.

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
{"title":"...","type":"ticket","status":"new","priority":"medium","parent_id":183,"description":"...","acceptance_criteria":"..."}
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
- **Auth**: Multi-user (email + bcrypt + TOTP), HttpOnly session cookies (24h, SameSite=Lax, Secure)
- **Deployment**: Docker on csb1 (personal) + BYTEPOETS Hetzner server, behind Cloudflare DNS
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
FleetCom Server (Docker)
  ├── POST /api/heartbeat         (bearer token auth, agents push here)
  ├── POST /api/container-events  (real-time container lifecycle events)
  ├── GET  /api/events            (SSE stream, filtered per user's host access)
  ├── POST /login                 (email + password → TOTP if enabled → session cookie)
  ├── GET  /setup-totp            (mandatory TOTP setup on first login)
  ├── POST /forgot-password       (email-based password reset)
  ├── GET  /                      (static HTML + Alpine.js dashboard)
  ├── /api/users/*                (admin: user CRUD, host permissions, TOTP reset)
  ├── /api/auth/*                 (self-service: password, TOTP, sessions)
  └── SQLite (hosts, containers, agents, users, sessions, tokens, ...)
        │
        ▼
Browser (Alpine.js + SSE — reactive, no polling)
```

## Auth & User Management

### Login flow
1. Email + password → bcrypt verify
2. If TOTP enabled → intermediate TOTP form (5min pending token)
3. Session cookie created (24h, HttpOnly, Secure, SameSite=Lax)

### Mandatory TOTP
All users must set up TOTP on first login. Users without TOTP are redirected to `/setup-totp` and blocked from all other routes.

### Password reset
- `POST /forgot-password` → always same response (no enumeration)
- Email with reset link (60min TTL, single-use SHA-256 token)
- If SMTP not configured, link logged to stdout (dev mode)
- All sessions invalidated on password reset

### Admin features
- Create/disable/delete users (`GET/POST /api/users`, `PUT /api/users/{id}/status`)
- Reset user's TOTP (`POST /api/users/{id}/reset-totp`)
- Invalidate user sessions (`DELETE /api/users/{id}/sessions`)
- Per-user host permissions (`GET/POST/DELETE /api/users/{id}/hosts`)

### Self-service
- Change password (`POST /api/auth/password`) — invalidates other sessions
- TOTP setup/disable (`GET /api/auth/totp/setup`, `POST .../enable`, `POST .../disable`)
- Session management (`GET /api/auth/sessions`, `DELETE /api/auth/sessions/{id}`)

### Per-user host permissions
- Admins see all hosts. Regular users see only assigned hosts.
- New users have no host access by default — admin must grant.
- Filtering applied to: host list, SSE events, host configs, history.
- Admin UI: Settings > Users > click "Hosts" per user.

### Rate limiting
5 attempts per 10 minutes per IP + identity. Scopes: login, totp-verify, forgot, reset.

### Environment variables (auth)
- `FLEETCOM_ADMIN_EMAIL` + `FLEETCOM_ADMIN_PASSWORD` — seed admin on first run (empty users table)
- `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM` — for password reset emails
- `APP_BASE_URL` — base URL for reset links (default: http://localhost:8090)

## UI Features

### Theme toggle
Dark / light / auto (OS preference). Toggle in header-right. Persisted in localStorage.

### Instance branding
- **Instance label**: Set via Settings > Config > Branding or `FLEETCOM_INSTANCE_LABEL` env var. Shows in header next to "FleetCom".
- **Org logo**: Upload via Settings > Config > Branding. Stored as base64 in settings table. Shows in header.
- **Domain**: Shown in header when instance label is set.

### Icon presets
- Upload transparent PNGs in Settings > Icons. Assign per-host in Settings > Hosts.
- **Export**: Settings > Icons > Export ZIP — downloads all icons as ZIP bundle with manifest.
- **Import**: Settings > Icons > Import ZIP — adds icons from ZIP. Option to overwrite duplicates.

### Share links
Read-only dashboard links with optional expiry. Settings > Sharing.

## Deployments

### Personal (fleet.barta.cm)
- **Host**: csb1 (Hetzner VPS, NixOS)
- **Config**: ~/Code/nixcfg/hosts/csb1/configuration.nix
- **Secrets**: agenix (`~/Code/nixcfg/secrets/csb1-fleetcom-env.age` → `/run/agenix/csb1-fleetcom-env`)
- **Deploy**: Push to main → CI builds image → auto-deploys via SSH + `/etc/fleetcom-deploy.sh`
- **Docker**: `/home/mba/docker/fleetcom/docker-compose.yml`

### BYTEPOETS (fleet.bytepoets.com)
- **Host**: Hetzner VPS at 5.75.130.206 (Ubuntu, shared with PMO)
- **Infra repo**: BYTEPOETS/infracore (docker-compose, nginx configs)
- **Secrets**: `/home/service-user/.fleetcom.env` (mode 600)
- **Deploy**: Push to main → CI builds image → manually trigger "Deploy to BYTEPOETS" workflow
- **Nginx**: `/etc/nginx/sites-available/fleet.bytepoets.com` (SSE-aware proxy + Let's Encrypt TLS)
- **SSH**: `ssh -i ~/.ssh/BP_OPS_Server_SSH_Key service-user@5.75.130.206`

### Initial admin setup (new instance)
1. Set `FLEETCOM_ADMIN_EMAIL` and `FLEETCOM_ADMIN_PASSWORD` in the env file
2. Start/recreate the container — admin user seeded automatically
3. Log in → forced TOTP setup → save to authenticator
4. The seed env vars are ignored after first user exists

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
- **Source the file** (`source ~/Secrets/PPMAPIKEY.env`) then use the env var

If you need to verify a secret exists: `test -n "$PPMAPIKEY"`. **Never print the value.**

## Security

- All communication over HTTPS (Cloudflare edge TLS)
- Bosun → server: per-host bearer token (SHA-256 hashed in DB)
- Browser → server: email + bcrypt password + mandatory TOTP, HttpOnly session cookies
- Per-user host permissions (admins bypass, regular users see only assigned hosts)
- Rate limiting on all auth endpoints (5/10min)
- Password reset tokens: SHA-256 hashed, single-use, 60min TTL
- Session invalidation on password change/reset/user disable
- Host communication over Tailscale mesh where possible
- Audit logging on all auth events

## Dependencies

External dependencies are acceptable when they provide clear value — evaluate each on merit rather than defaulting to "no deps." Self-host JS libraries (Alpine.js, Lucide) to avoid CDN reliance. No npm/build step required; keep the single-HTML-file architecture.

## Build & Run

```bash
# Backend dev (seeds admin if FLEETCOM_ADMIN_EMAIL/PASSWORD set and no users exist)
cd backend && FLEETCOM_ADMIN_EMAIL=admin@test.com FLEETCOM_ADMIN_PASSWORD=test123 go run .

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
- BP infra: ~/Code/infracore (BYTEPOETS server docker-compose + nginx)
- BP PMO: ~/Code/bp-pm (auth patterns reference)
