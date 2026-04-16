# Development

## Prerequisites

- Go 1.25+
- Docker (for container builds)
- Optional: Nix (`nix-shell -p go` works)

## Project Structure

```
fleetcom/
├── backend/
│   ├── cmd/server/main.go          # Entry point, route wiring, admin seed
│   ├── internal/
│   │   ├── api/                    # HTTP handlers
│   │   │   ├── hosts.go            # Host/token CRUD
│   │   │   ├── events.go           # SSE endpoint (per-user filtered)
│   │   │   ├── heartbeat.go        # Agent heartbeat ingestion
│   │   │   ├── container_events.go # Agent container event ingestion
│   │   │   ├── users.go            # User management + self-service API
│   │   │   ├── access.go           # Per-user host access helpers
│   │   │   ├── settings.go         # Config, branding, org logo
│   │   │   ├── hostconfig.go       # Host icons + image presets
│   │   │   ├── presets_bundle.go   # Icon ZIP export/import
│   │   │   ├── share.go            # Share links
│   │   │   ├── history.go          # Status history
│   │   │   ├── ignored.go          # Ignored entities
│   │   │   └── pages.go            # HTML page serving
│   │   ├── auth/
│   │   │   ├── auth.go             # Login, logout, TOTP, session middleware, admin seed
│   │   │   ├── ratelimit.go        # In-memory rate limiter
│   │   │   ├── reset.go            # Password reset handlers
│   │   │   └── smtp.go             # Email sending
│   │   ├── db/
│   │   │   ├── db.go               # Schema, migrations, Store
│   │   │   ├── users.go            # User CRUD, TOTP pending, password reset tokens, host access
│   │   │   ├── sessions.go         # Session CRUD with user_id
│   │   │   ├── heartbeat.go        # Host/container/agent upsert + AllHosts/HostsForUser
│   │   │   ├── tokens.go           # Agent bearer token management
│   │   │   ├── hostconfig.go       # Host configs + image presets
│   │   │   ├── settings.go         # Key-value settings
│   │   │   ├── samples.go          # Status history samples
│   │   │   ├── share.go            # Share links
│   │   │   └── ignored.go          # Ignored entities
│   │   ├── sse/
│   │   │   └── hub.go              # SSE broadcast hub
│   │   └── version/
│   │       └── version.go          # Build metadata (ldflags)
│   ├── static/
│   │   ├── index.html              # Dashboard (Alpine.js, ~3800 lines)
│   │   ├── login.html              # Login page
│   │   ├── alpine.min.js           # Self-hosted Alpine.js
│   │   └── lucide/                 # Self-hosted Lucide icons
│   ├── Dockerfile
│   ├── go.mod
│   └── go.sum
├── agent/                          # Bosun agent (separate Go module)
├── docs/                           # This directory
├── CLAUDE.md                       # AI session context
└── docker-compose.yml              # Production compose (csb1/Traefik)
```

## Local Development

```bash
cd backend

# Run with admin seed (first time)
FLEETCOM_ADMIN_EMAIL=admin@test.com FLEETCOM_ADMIN_PASSWORD=test123 go run .

# Subsequent runs (admin already exists, seed is skipped)
go run .
```

Server runs on `http://localhost:8090`. Login, set up TOTP, explore.

## Building

```bash
cd backend

# Binary
CGO_ENABLED=0 go build -o ../dist/fleetcom-server ./cmd/server

# With version metadata
CGO_ENABLED=0 go build \
  -ldflags "-s -w -X .../version.Version=0.5.0 -X .../version.Commit=$(git rev-parse --short HEAD)" \
  -o ../dist/fleetcom-server ./cmd/server

# Docker
docker compose build
```

## Code Patterns

### Adding an API endpoint

1. Add the handler function in `internal/api/` (return `http.HandlerFunc`)
2. Wire the route in `cmd/server/main.go` (in the appropriate group: public, session-required, or admin-only)
3. If it returns host data, use `hostsForRequest(store, r)` instead of `store.AllHosts()` for access control

### Adding a database table

1. Add `CREATE TABLE IF NOT EXISTS` to the `schema` const in `internal/db/db.go`
2. For columns on existing tables, add `ALTER TABLE` to the `alterStmts` slice (idempotent)
3. Add CRUD methods on `*Store` in a new or existing file in `internal/db/`

### Auth middleware stack

Routes are grouped by auth level:
- **Public**: no auth (healthz, login, forgot-password, static files)
- **Session only**: `a.RequireSession` (setup-totp, /api/me)
- **Session + TOTP**: `a.RequireSession` + `auth.RequireTOTP` (all dashboard routes)
- **Admin**: nested `auth.RequireAdmin` (user management, token management)

### SSE filtering

When `hub.Broadcast("hosts", data)` fires (after heartbeat or container event), the `Events()` handler re-filters the data per user. Non-admins get `store.HostsForUser(userID)` instead of the broadcast payload.

## Linting

CI runs `gofmt`, `go vet`, and `staticcheck`. Run locally:

```bash
gofmt -l .            # check formatting
go vet ./...          # check for issues
staticcheck ./...     # extended checks (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
```

## CI/CD

On push to main:
1. Lint (gofmt + vet + staticcheck)
2. Build (linux/amd64)
3. Publish to GHCR (`latest` + `v{VERSION}` + `sha-{COMMIT}`)
4. Auto-deploy to csb1 (personal instance)

BYTEPOETS deploy is manual: `gh workflow run deploy-bytepoets.yml`
