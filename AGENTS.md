# AGENTS.md — FleetCom

> **Cross-tool agent onboarding doc.** Claude Code, Cursor, Aider, OpenAI
> Codex, Continue, and other agentic coding tools all read a file by this
> name. `CLAUDE.md` in this repo is a symlink to this file (FLEET-81).

> **Doctrine layering (post-Phase-6, 2026-05-15)** — kernel auto-loaded; this file is the fleetcom delta. Slash commands `/dev /secrets /nix /ops /ppm /style /incident /inspr` load deeper context on demand. See [AGENTS-INDEX.md](https://github.com/markus-barta/inspr-modules/blob/main/docs/AGENTS-INDEX.md) for the architecture.
>
> Pure rules (security/secrets-output, git/safety, process/critical-thinking, etc.) live upstream in `inspr-modules` (kernel + domain packs). This file holds project-specific stuff: API endpoints, architecture, deploy procedures, debug entry points, project-management process.

**Read `docs/DEPLOYMENT.md` at every session start.** Every deploy must bump the patch version in `.github/workflows/ci.yml` — never deploy the same version twice.

<!-- KERNEL-MIRROR-BEGIN — auto-mirrored irreducible subset of inspr-modules/docs/AGENTS-KERNEL.md (INSPR-191). Edit upstream + bump submodule, then re-mirror here. For tools that read AGENTS.md but not the kernel via CLAUDE.md @-ref (Cursor, Aider, OpenCode, Codex CLI, Continue, etc.). -->

## Universal must-knows (kernel mirror)

- **Identity**: Markus Barta, `markus@barta.com`, `markus-barta` on GitHub. Never invent placeholders.
- **Workspace**: `~/Code/`. Repos under `github.com/markus-barta/<name>`. Third-party clones go to `~/Projects/3rdparty/`.
- **Time awareness**: Run `date` before any time-of-day-coded greeting/farewell ("good evening", 🌙 / ☀️). Knowing the date alone tells you nothing about morning/night. Prefer time-neutral closings ("cheers", "until next time") if a check would be disruptive.
- **Style**: telegraph, dense, low-fluff. **Long** answers: TL;DR at start AND end. **Short**: TL;DR at end only. **Very short**: omit TL;DR.
- **Pacing**: ONE STEP AT A TIME for interactive procedures (agenix, ssh, paimos auth, rotation flows). Wait for explicit "done" before next step. Never dump 5- or 10-step playbooks.
- **Secrets**: NEVER `cat / Read / head / tail / less / bat / xxd / od / sed / grep` files in `~/.inspr/secrets/agents/`, `~/Secrets/`, `~/.ssh/<not-pub>`, `/run/agenix/`, `/run/secrets/`, or any `*.env` / `*.age` / `*.gpg` / `id_*` / `*_rsa` / `*_ed25519`. Source via `( set -a; source FILE; cmd; set +a )`. NEVER run `direnv export`, `direnv status`, `set`, `declare -x/-p`, `compgen -e`, `export -p`, bare `env` / `printenv`, `docker exec ... cat env`, `kubectl describe configmap` after env expansion. If a secret appears in output: **STOP**, name affected vars (not values), rotate before continuing.
- **Git**: never `reset --hard` / `clean -f` / `restore .` / `branch -D` / `rm` unless asked. Never `--force` push main. Never `--no-verify` / `--no-gpg-sign` / `--amend` unless asked. Never commit secrets (passwords, API keys, .env with real creds, decrypted .age content). `git diff` + `git status` before every commit.
- **Files & ops**: use `trash` not `rm -rf`. Don't delete or rename unexpected items — STOP and ask. Touch encrypted files only with explicit permission. **NEVER build NixOS configs on macOS** (build remotely via ssh; macOS HM CAN build locally). Never create new `.md` files unless asked.
- **Naming**: **BYTEPOETS** always all-caps (registered wordmark). **`.cm`** TLD intentional, never auto-correct to `.com`. **INSPR** is the umbrella; Paimos / FleetCom / future tools are inside it.

For full kernel + domain packs: see [`inspr-modules/docs/AGENTS-KERNEL.md`](https://github.com/markus-barta/inspr-modules/blob/main/docs/AGENTS-KERNEL.md). Claude Code agents: run `/inspr` for the TL;DR map of slash commands.

<!-- KERNEL-MIRROR-END -->

## Project

**FleetCom** — Fleet management & agent monitoring platform.
Central hub for managing DSC-AI agent fleet and NixOS infrastructure. Replaces NixFleet (legacy).

- **Domains**: fleet.barta.cm (personal), fleet.bytepoets.com (BYTEPOETS)
- **PPM Project**: FleetCom (project ID: 4, key: FLEET)
- **PPM Epic**: FLEET-1 (FleetCom MVP)
- **Infra repo**: BYTEPOETS/infracore (BP server docker-compose + nginx)

## First-contact orientation: discover the API in one curl

Every public route, auth method, and required scope is documented at
`GET /api/info` (FLEET-80). It's public, unauthenticated, and the
single source of truth — kept honest by a CI test that fails if any
registered chi route is missing from the catalog.

```sh
curl -s https://fleet.barta.cm/api/info | jq
```

Returns `{version, commit, build_time, auth_methods, endpoints, links}`.
Use it before grepping the source.

## Project Management: PPM is the Single Source of Truth

All task tracking, planning, and status updates happen in **PPM** at `https://pm.barta.cm`.

### Rules

- **Before starting work**: Check PPM for a backing ticket. If none exists, create one first.
- **Planning**: Create epics and tickets in PPM. Do not create local backlog files.
- **Status updates**: Update ticket status in PPM as work progresses (new → in-progress → done).
- **Time tracking**: Start timer when beginning work, stop when done. mba user_id=2.
- **Done means done**: Mark tickets as `done` in PPM only when acceptance criteria are met.
- **Reference style**: Always refer to tickets by their human-visible key (e.g. `FLEET-79`, `FLEET-80`, `FLEET-81`) in chat, commits, branch names, and PR titles. The numeric DB id (e.g. `1326`) is for API calls only.

### PPM API Access

Auth: Bearer token via `$PPMAPIKEY` environment variable (loaded from `~/Secrets/ppm/PPMAPIKEY.env`).
Source it first: `source ~/Secrets/ppm/PPMAPIKEY.env` — then use `$PPMAPIKEY`.
All requests: `curl -s -H "Authorization: Bearer $PPMAPIKEY" https://pm.barta.cm/api/...`

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
- **Auth**: Multi-user (email + bcrypt + TOTP), HttpOnly session cookies (24h, SameSite=Lax, Secure). User-issued `fleet_pat_` API tokens for programmatic agents (FLEET-79).
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
  ├── POST /api/heartbeat         (per-host bearer token, agents push here)
  ├── POST /api/container-events  (real-time container lifecycle events)
  ├── GET  /api/info              (FLEET-80: public self-describing catalog)
  ├── GET  /api/hosts             (FLEET-79: session OR fleet_pat_ token w/ read:hosts)
  ├── GET  /api/agents            (FLEET-79: session OR fleet_pat_ w/ read:agents)
  ├── GET  /api/events            (SSE stream, filtered per user's host access)
  ├── POST /login                 (email + password → TOTP if enabled → session cookie)
  ├── GET  /setup-totp            (mandatory TOTP setup on first login)
  ├── POST /forgot-password       (email-based password reset)
  ├── GET  /                      (static HTML + Alpine.js dashboard)
  ├── /api/users/*                (admin: user CRUD, host permissions, TOTP reset)
  ├── /api/auth/*                 (self-service: password, TOTP, sessions, api-tokens)
  └── SQLite (hosts, containers, agents, users, sessions, tokens, user_api_tokens, ...)
        │
        ▼
Browser (Alpine.js + SSE — reactive, no polling)
```

## API auth methods

Three flows can authenticate against the server. Full details (and the
canonical scope list) in `GET /api/info`.

| Type | Used by | How |
|------|---------|-----|
| `session` | Browsers | `fleetcom_session` cookie + mandatory TOTP via `/login` |
| `api_token` | Agents, scripts | `Authorization: Bearer fleet_pat_<64hex>` (FLEET-79) |
| `agent_bearer` | Bosun on each host | `Authorization: Bearer <hosttoken>` for write-side `/api/heartbeat`, `/api/container-events`, `/api/agent-events`, `/api/bridges/register`, `/api/command-results` |
| `share_token` | Read-only viewers | URL path `/s/{token}` (no header) |

API tokens (`fleet_pat_…`) are scoped read-only and inherit the owning
user's `user_host_access` rows — admins see all hosts, regular users
see only granted hosts. Available scopes (v1): `read:hosts`,
`read:agents`, `read:hardware`, `read:info`.

## Querying fleet inventory programmatically

### One-time: mint a token from the dashboard

1. Sign in at `https://fleet.barta.cm/`.
2. **Settings → Account → API Tokens**.
3. Pick a label (e.g. `agents@dsc0`), select scopes, choose an expiry
   (the UI strongly discourages "Never" with a yellow warning banner).
4. Copy the token from the one-time modal — the plaintext is shown
   exactly once and never retrievable again.
5. Store it in `~/Secrets/<NAME>.env` so the filename is the env-var
   name (project convention): e.g.
   `~/Secrets/FLEETCOM_API_TOKEN.env` exporting `FLEETCOM_API_TOKEN=fleet_pat_…`.

### Then: source and curl

```sh
source ~/Secrets/FLEETCOM_API_TOKEN.env

# List accessible hosts (filtered by your user's host-access scope)
curl -s -H "Authorization: Bearer $FLEETCOM_API_TOKEN" \
  https://fleet.barta.cm/api/hosts | jq '.[].hostname'

# Hardware snapshot for one host
curl -s -H "Authorization: Bearer $FLEETCOM_API_TOKEN" \
  https://fleet.barta.cm/api/hosts/dsc0/hardware | jq

# All agents across accessible hosts
curl -s -H "Authorization: Bearer $FLEETCOM_API_TOKEN" \
  https://fleet.barta.cm/api/agents | jq '.[] | {host, name, status}'

# Single agent detail with recent turns
curl -s -H "Authorization: Bearer $FLEETCOM_API_TOKEN" \
  https://fleet.barta.cm/api/agents/dsc0/merlin | jq
```

Failure modes (audit-logged on the server):

- `401 unauthorized` — token unknown, revoked, expired, or owning user disabled
- `403 forbidden: token lacks scope read:agents` — token doesn't have the route's required scope
- `429 too many attempts` — 10 failures / 10 min per (IP, prefix); back off

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
- API tokens (`GET/POST/DELETE /api/auth/api-tokens` — FLEET-79)

### Per-user host permissions
- Admins see all hosts. Regular users see only assigned hosts.
- New users have no host access by default — admin must grant.
- Filtering applied to: host list, SSE events, host configs, history, API token reads.
- Admin UI: Settings > Users > click "Hosts" per user.

### Rate limiting
5 attempts per 10 minutes per IP + identity. Scopes: `login`, `totp-verify`, `forgot`, `reset`. The `api-token-auth` scope is bumped to 10/10min — agents legitimately retry more often than humans typing passwords.

### Environment variables (auth)
- `FLEETCOM_ADMIN_EMAIL` + `FLEETCOM_ADMIN_PASSWORD` — seed admin on first run (empty users table)
- `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM` — for password reset emails
- `APP_BASE_URL` — base URL for reset links and `/api/info` `links.dashboard` (default: http://localhost:8090)

## How to add a host

1. As an admin, `POST /api/hosts` with `{"hostname": "newhost"}`. The
   response includes a per-host bearer token (`token`) shown **once**.
2. Store the token on the host as `HOST_TOKEN` (or via your config
   manager — agenix on NixOS, env file with mode 0600 elsewhere).
3. Deploy the bosun container (see "Bosun Deployment" below) with that
   token in `FLEETCOM_TOKEN`. First heartbeat hits `POST /api/heartbeat`
   and the host appears on the dashboard within ~1s via SSE.
4. Token rotation: `POST /api/hosts/{hostname}/token` mints a fresh
   one and invalidates the old. Update the host's env file and
   `docker compose up -d` to pick up the new value.

## Bosun protocol (write-side)

Bosun runs on every managed host as a Docker container and reaches the
server via three flows. All three use the per-host bearer token in
`Authorization: Bearer …`.

### Heartbeat (every 60s, enriched)
`POST /api/heartbeat` body:
```json
{
  "hostname": "dsc0",
  "os": "...", "kernel": "...", "uptime_seconds": 12345,
  "agent_version": "0.1.0",
  "boot_id": "...", "deployment_shape": "docker+watchtower",
  "containers": [{"name":"...","image":"...","state":"running","health":"healthy","restart_count":0,"started_at":"...","exit_code":0,"oom_killed":false}],
  "agents": [{"name":"merlin","agent_type":"openclaw","status":"idle","last_seen":"..."}],
  "hw_live": {...}, "fastfetch_json": {...}
}
```

The response carries pending commands (FLEET-60 channel): bosun pulls
them, executes locally, and replies with results to the next channel.

### Real-time container events
`POST /api/container-events` — streamed from Docker socket on every
`die`, `restart`, `oom`, `health_status` event so the dashboard
reflects faults within ~1s without waiting for the next heartbeat.

### Agent observability events
`POST /api/agent-events` — agent-bridge relays per-turn events
(turn started, first token, replied, tool invocations, errors). See
`docs/AGENT-OBSERVABILITY.md`.

### Command results
`POST /api/command-results` — bosun's reply channel for the pull-based
command queue (host.reboot, agent.update, etc.).

## Debug entry points

```sh
# Server logs (deployed via docker)
ssh csb1 'docker logs --tail=200 -f fleetcom'
ssh service-user@5.75.130.206 'docker logs --tail=200 -f fleetcom'

# Bosun logs (per host)
ssh dsc0 'docker logs --tail=200 -f fleetcom-bosun'

# Tail the SSE stream as a logged-in user
curl -N -H "Cookie: fleetcom_session=$SESSION" https://fleet.barta.cm/api/events

# Inspect the SQLite DB (read-only is safest)
sqlite3 -readonly /home/mba/docker/fleetcom/data/fleetcom.db
sqlite> .tables
sqlite> SELECT hostname, last_seen, deployment_shape FROM hosts;
sqlite> SELECT id, label, prefix, scopes, last_used_at, expires_at, revoked_at FROM user_api_tokens;

# Verify a per-host bearer token is wired up correctly
curl -X POST -H "Authorization: Bearer <hosttoken>" \
  https://fleet.barta.cm/api/heartbeat \
  -H "Content-Type: application/json" \
  -d '{"hostname":"dsc0"}'
# 200 = token is valid; 401 = token is wrong/missing
```

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

### API Tokens (FLEET-79)
Settings > Account > API Tokens. Mint, list, revoke `fleet_pat_` tokens. Plaintext shown once on creation.

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
  labels:
    - com.centurylinklabs.watchtower.enable=true
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock:ro
    - /proc:/host/proc:ro
    - /sys:/host/sys:ro
    - /etc/os-release:/host/etc/os-release:ro
    - /etc/mtab:/host/etc/mtab:ro
  environment:
    - FLEETCOM_URL=https://fleet.barta.cm
    - FLEETCOM_TOKEN=${HOST_TOKEN}
    - FLEETCOM_HOSTNAME=${HOSTNAME}
    - FLEETCOM_AGENTS=${AGENTS_JSON}
    - WATCHTOWER_URL=http://watchtower:8080
    - WATCHTOWER_TOKEN=${WATCHTOWER_TOKEN}

watchtower:
  image: containrrr/watchtower:latest
  restart: unless-stopped
  volumes:
    - /var/run/docker.sock:/var/run/docker.sock
  environment:
    - WATCHTOWER_LABEL_ENABLE=true
    - WATCHTOWER_CLEANUP=true
    - WATCHTOWER_POLL_INTERVAL=86400
    - WATCHTOWER_HTTP_API_UPDATE=true
    - WATCHTOWER_HTTP_API_TOKEN=${WATCHTOWER_TOKEN}
```

`WATCHTOWER_TOKEN` is a random string (any 20+ chars) shared between bosun and
watchtower on each host. Watchtower only manages containers that carry the
`com.centurylinklabs.watchtower.enable=true` label, so unrelated host
containers are untouched.

### Two deployment modes

**Standalone** — host has no watchtower. Bring up both services:
```
docker compose --profile standalone up -d
```
The bundled watchtower sidecar is gated behind the `standalone` profile, so
plain `docker compose up -d` skips it.

**Piggyback on an existing host watchtower** — default path when the host
already runs watchtower (common on multi-service NixOS hosts):
```
docker compose up -d   # skips the sidecar
```
Enable HTTP API on the existing watchtower with
`WATCHTOWER_HTTP_API_UPDATE=true` and a matching token, then set
`WATCHTOWER_URL=http://<existing-watchtower-container>:8080` in bosun's env so
the "Update now" button from the dashboard hits the right daemon.

### Image names (compatibility)

CI currently publishes to **both** `ghcr.io/markus-barta/fleetcom-bosun` and
`ghcr.io/markus-barta/fleetcom-agent` for the same digest. The `-agent` name
is kept temporarily so hosts still pointing at the pre-rename image keep
receiving updates. New hosts and existing hosts being reconfigured should
switch to `fleetcom-bosun`.

## Secret Safety

> Canonical rules live upstream — see `inspr-modules/docs/AGENTS-CORE.md` topic `security/secrets-output` (and the much fuller `incident-response/secret-leak` topic) for the full doctrine. Quick fleetcom-specific reminder: PPM credentials live at `~/Secrets/ppm/PPMAPIKEY.env` — `source` it, never `cat` it. To verify a secret is set: `test -n "$PPMAPIKEY"`.

## Security

- All communication over HTTPS (Cloudflare edge TLS)
- Bosun → server: per-host bearer token (SHA-256 hashed in DB)
- Browser → server: email + bcrypt password + mandatory TOTP, HttpOnly session cookies
- Agent → server: user-issued `fleet_pat_` tokens (FLEET-79) — SHA-256 hashed, scope-checked, owner-revocable, expiry-optional with UI warning
- Per-user host permissions (admins bypass, regular users see only assigned hosts)
- Rate limiting on all auth endpoints (5/10min; api-token-auth bumped to 10/10min)
- Password reset tokens: SHA-256 hashed, single-use, 60min TTL
- Session invalidation on password change/reset/user disable
- Host communication over Tailscale mesh where possible
- Audit logging on all auth events (including api_token_created / _revoked / _auth_failed)
- Bridge → gateway WS: shared-secret short-circuit (FLEET-134). Bosun bind-mounts the gateway's `/run/secrets/gateway-token` into the bridge container at the same path; bridge sends as `auth.token`; gateway's `roleCanSkipDeviceIdentity(role=operator, sharedAuthOk)` lets it through without device-pairing. Read-only intent; co-located blast radius. See `docs/PAIRING-SECURITY-MODEL.md`.

## Invariants worth knowing (non-obvious)

- **Agent names are operator-asserted, no defaults (FLEET-149).** `bridge.install` rejects empty `agent_names`; the agent-bridge container refuses to start with empty `BRIDGE_AGENT_NAMES`. `bridge.reinstall` (FLEET-131) preserves the existing container's `BRIDGE_AGENT_NAMES` env across recreate. Never invent a default — agent identity must come from the host.
- **`/healthz` probes the DB (FLEET-148).** A static "ok" responder hid an outage during the v1.0.10 SetMaxOpenConns deadlock; `/healthz` now does `SELECT 1` with a 2s timeout. External healthchecks can rely on it.
- **Onboarding banner counts only agent-bearing hosts (FLEET-155).** Pure-infra hosts (no agents reported in heartbeats) are skipped from the "needs gateway pairing" / "ready to deploy a bridge" buckets.

## Dependencies

External dependencies are acceptable when they provide clear value — evaluate each on merit rather than defaulting to "no deps." Self-host JS libraries (Alpine.js, Lucide) to avoid CDN reliance. No npm/build step required; keep the single-HTML-file architecture.

## Build & Run

```bash
# Backend dev (seeds admin if FLEETCOM_ADMIN_EMAIL/PASSWORD set and no users exist)
cd backend && FLEETCOM_ADMIN_EMAIL=admin@test.com FLEETCOM_ADMIN_PASSWORD=test123 go run ./cmd/server

# Build
cd backend && go build -o fleetcom-server ./cmd/server

# Tests (includes the FLEET-80 drift-protection guard)
cd backend && go test ./...

# Docker
docker compose build && docker compose up -d
```

## Cross-references

In-repo docs (read these for subsystem detail):

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — system layout, data flow, why-we-chose
- [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) — how to ship a release, version-bump rules, runbooks per host
- [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) — local dev loop, conventions
- [`docs/ADMIN-GUIDE.md`](docs/ADMIN-GUIDE.md) — operator-facing reference (settings, host onboarding, drawer UX)
- [`docs/AGENT-OBSERVABILITY.md`](docs/AGENT-OBSERVABILITY.md) — `/api/agent-events` shape and dashboard surfacing
- [`docs/AGENT-BRIDGE-PAIRING.md`](docs/AGENT-BRIDGE-PAIRING.md) — OpenClaw gateway + bridge pairing flow
- [`docs/OPS.md`](docs/OPS.md) — bosun release runbook, watchtower piggyback, troubleshooting

External:

- NixFleet (predecessor): ~/Code/nixfleet — patterns to adopt, not code to fork
- DSC infra: ~/Code/dsccfg
- NixCfg infra: ~/Code/nixcfg
- PPM tool: ~/Code/ppm
- BP infra: ~/Code/infracore (BYTEPOETS server docker-compose + nginx)
- BP PMO: ~/Code/bp-pm (auth patterns reference)
