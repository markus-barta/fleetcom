# Architecture

## Overview

FleetCom is a fleet management and agent monitoring platform. It provides real-time visibility into hosts, Docker containers, and AI agents across NixOS and Linux infrastructure.

**Vision**: Internal ops tool → customer-facing dashboard → "one-click hire your CTO" portal.

## System Design

```
Hosts (dsc0, csb0, csb1, hsb0, ...)
  └── fleetcom-bosun (Docker container)
        ├── Docker event stream (real-time: die, restart, oom, health_status)
        │     → POST /api/container-events
        └── Periodic heartbeat (60s)
              → POST /api/heartbeat
        │
        ▼
FleetCom Server (Docker)
  ├── /api/heartbeat, /api/container-events  (agent auth: per-host bearer tokens)
  ├── /api/events                            (SSE, filtered per user's host access)
  ├── /login, /login/totp, /setup-totp       (multi-user auth)
  ├── /forgot-password, /reset/{token}       (password reset)
  ├── /api/users/*, /api/auth/*              (admin + self-service)
  ├── /                                      (Alpine.js dashboard)
  └── SQLite (WAL mode)
        │
        ▼
Browser (Alpine.js + SSE — reactive, no polling)
```

## Tech Stack

| Layer | Choice | Why |
|-------|--------|-----|
| Backend | Go (stdlib net/http + chi router) | Fast, single binary, no runtime |
| Database | SQLite via modernc.org/sqlite | Pure Go (no CGO), zero ops, WAL for concurrent reads |
| Real-time | Server-Sent Events (SSE) | Read-only dashboard, native EventSource API, works through Cloudflare |
| Frontend | Single HTML file, Alpine.js + Lucide icons | 17KB, no build step, no npm, reactive via HTML attributes |
| Auth | bcrypt + TOTP (mandatory), 24h sessions | Multi-user, rate-limited, per-user host permissions |
| Deployment | Docker, GHCR images | CI builds on push to main |

## Key Design Decisions

### SSE over WebSocket
Dashboard is server → browser only. SSE is purpose-built for this: native `EventSource` API, auto-reconnects, works through Cloudflare with zero config. Upgrade to WebSocket only if bidirectional comms needed (Phase 2 orchestration).

### Alpine.js over React/Vue/Svelte
17KB, single script tag, zero build step. The entire frontend is one HTML file you can read top to bottom. Add complexity when the current approach hurts, not before.

### Per-user SSE filtering
The SSE hub broadcasts all data to all subscribers. Each client's `Events()` handler filters the broadcast based on the user's host access before writing to the wire. Admins get unfiltered data.

### Agent auth separate from user auth
Bosun agents use per-host bearer tokens (SHA-256 hashed in DB). Users use email + password + TOTP with session cookies. The two auth paths never intersect.

## Database Schema

### Core tables
- **hosts** — hostname, OS, kernel, uptime, agent_version, last_seen
- **containers** — per-host Docker containers with state, health, restart count
- **agents** — AI agents per host (name, type, status)
- **container_events** — lifecycle events (die, restart, oom)
- **status_samples** — historical status data for timeline strips

### Auth tables
- **users** — email, bcrypt hash, role (admin/user), status (active/inactive/deleted), TOTP
- **sessions** — token, user_id, expires_at (24h)
- **totp_pending** — short-lived tokens for 2FA login step (5min)
- **password_reset_tokens** — SHA-256 hashed, single-use, 60min TTL
- **user_host_access** — junction table (user_id, host_id)

### Config tables
- **tokens** — per-host bearer tokens for agent auth
- **settings** — key-value config (heartbeat interval, instance label, org logo)
- **share_links** — read-only dashboard links with optional expiry
- **image_presets** — icon images (name, mime, blob)
- **host_configs** — per-host dashboard config (icon, comment)
- **ignored_entities** — user-hidden hosts/containers

## Auth Model

### Login flow
1. `POST /login` — email + password → bcrypt verify
2. If TOTP enabled → intermediate form with pending token (5min TTL)
3. `POST /login/totp` — verify TOTP code → create 24h session
4. If TOTP not enabled → redirect to `/setup-totp` (mandatory)

### Permissions
- **Admins**: see all hosts, manage users, manage tokens, manage branding
- **Users**: see only hosts assigned via `user_host_access`, self-service account management
- New users have zero host access by default

### Rate limiting
In-memory, 5 attempts per 10 minutes, dual-key (IP + identity). Scopes: login, totp-verify, forgot, reset. Resets on success, lazy-prunes stale entries.

## Roadmap

| Phase | What |
|-------|------|
| **Phase 2** | Alerting (Telegram on down), OpenRouter balance, basic orchestration (restart/deploy) |
| **Phase 3** | Config editor, cost tracking, backup status, audit log, per-customer dashboards |
| **Future** | Replaces NixFleet entirely, becomes customer-facing product feature |
