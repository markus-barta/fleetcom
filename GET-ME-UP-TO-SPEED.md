# Get Me Up to Speed — FleetCom

This document captures everything a new session needs to know to work on FleetCom effectively. Read this first.

---

## Who is Markus?

Markus Barta (@markus-barta) is the sole admin and infrastructure owner. He runs:

- **DSC-AI**: An AI agent deployment business targeting Austrian/EU SMEs
- **NixCfg**: His personal NixOS/macOS fleet (home servers, cloud servers, workstations)
- **PPM**: His self-built project management tool at pm.barta.cm

Markus is a senior infrastructure engineer based in Austria. He uses NixOS for all servers, manages secrets with agenix, and runs a Headscale/Tailscale mesh network across all hosts. His work style is telegraph-concise, he values security first, and he tracks everything in PPM.

**SSH key**: `~/.ssh/dsccfg_deploy` (Ed25519, used for all DSC hosts)
**PPM API**: `$PPMAPIKEY` env var, base URL `https://pm.barta.cm`

## Who is David?

David Scheutz is the first customer/user. Staff Software Engineer at Keeta (blockchain/payments), originally from Graz, Austria, now US West Coast. His fiancée is Nataly Nariño (Colombian, @natalynarino on Instagram).

David has an AI agent called **Ocean** running on dsc0 — a Cloud CTO prototype. He communicates with Ocean via Telegram. Nataly will get her own agent called **Adi** — a social media assistant with Karol G / Latina energy.

## What is FleetCom?

FleetCom is the **central hub for managing the entire agent fleet and NixOS infrastructure**. It replaces NixFleet (legacy, being decommissioned).

- **Product name**: FleetCom
- **Domain**: fleet.barta.cm (+ redirect fleetcom.barta.cm)
- **Repo**: markus-barta/fleetcom (private)
- **PPM Epic**: DSC26-52

### Vision

Internal ops tool now → customer-facing dashboard later → eventually "one-click hire your CTO" portal.

### MVP Scope

A single web page showing all hosts with status indicators:

```
Host       System   Containers          Agents
─────────────────────────────────────────────────
dsc0       🟢       🟢 openclaw-gateway  🟢 Ocean
hsb0       🟢       🟢 openclaw-gateway  🟢 Merlin  🟢 Nimue
csb0       🟢       🟢🟢🟢 (3)           —
csb1       🟢       🟢🟢🟢🟢 (6)         —
hsb1       🟢       🟢🟢🟢 (4)           —
hsb2       🟢       —                    —
hsb8       🟢       —                    —
```

### Architecture decided

- **Backend**: Go (stdlib net/http + chi + SQLite via modernc.org/sqlite, pure Go, no CGO)
- **Real-time**: Server-Sent Events (SSE) — server pushes updates to browser the instant a heartbeat arrives. No polling.
- **Frontend**: Single HTML file, Alpine.js (17KB, reactive data binding via HTML attributes, no build step, no npm). Self-hosted Alpine.js (not CDN) for zero external runtime dependencies.
- **Agent**: Go daemon in Docker container — Docker socket event stream + periodic heartbeat (replaces legacy shell script)
- **Auth**: Password + TOTP, session cookies (HttpOnly, SameSite=Lax, Secure), per-host bearer tokens for agents
- **Deployment**: Docker on csb1, Cloudflare DNS (edge TLS)
- **Security**: All HTTPS, Tailscale mesh for host comms, no external dependencies for auth, per-host bearer tokens
- **Database**: SQLite (WAL mode) with hosts, containers, agents, sessions, tokens tables
- **Inspired by**: NixFleet (~/Code/nixfleet, ~20K lines Go) — adopt patterns, don't fork

### Why SSE over WebSocket / polling

- Dashboard is read-only (server → browser only) — SSE is purpose-built for this
- Native browser `EventSource` API — auto-reconnects, no library needed
- Works through Cloudflare with zero special config
- Server broadcasts to all connected browsers when any heartbeat arrives — instant UI update
- Upgrade to WebSocket later if bidirectional comms are needed (Phase 2 orchestration)

### Why Alpine.js over React/Vue/Svelte

- 17 KB, single script tag, zero build step, zero npm, zero node_modules
- Reactive data binding directly in HTML attributes — no virtual DOM, no JSX, no compilation
- The entire frontend is one HTML file you can read top to bottom
- When/if FleetCom grows into a multi-page app (Phase 3+), a heavier framework can be evaluated then
- Rule: add complexity when the current approach hurts, not before

### Phases

| Phase | What |
|-------|------|
| **MVP** | Host grid, 3-column status (system/containers/agents), heartbeat agents, TOTP auth |
| **Phase 2** | Alerting (Telegram on down), OpenRouter balance, basic orchestration (restart/deploy) |
| **Phase 3** | Config editor, cost tracking, backup status, audit log, per-customer dashboards |
| **Future** | Replaces NixFleet entirely, becomes customer-facing product feature |

---

## What is dsccfg?

`~/Code/dsccfg` — NixOS flake managing DSC-AI infrastructure.

| What | Details |
|------|---------|
| **Hosts** | dsc0 (Hetzner CPX31, Hillsboro OR, NixOS) |
| **Agents** | Ocean (David's CTO), Adi (Nataly's SoMe, planned) |
| **Modules** | common.nix, vps-base.nix, openclaw/default.nix |
| **Secrets** | Agenix (5 secrets: OpenRouter, Telegram, GitHub PAT, gateway token, Groq) |
| **Deploy** | `just deploy dsc0` (git push → SSH as mba → git pull → sudo nixos-rebuild switch) |
| **SSH** | Port 2222, key-only, mba+sudo (root disabled as of 2026-04-02), `~/.ssh/dsccfg_deploy` |
| **PPM** | DSC26 project (project ID: 2) |

### Recent changes (2026-04-02 session)

- Migrated from root SSH to mba+sudo (aligns with nixcfg hosts)
- Fixed heartbeat token drain ($37.50/day → near zero)
- Upgraded container to Node 24 (fixes memory search)
- Added Groq Whisper STT (voice message transcription)
- Updated Ocean's workspace (TOOLS.md, AGENTS.md)
- Wrote 458-line operational runbook
- Created Agent Baseline Spec in ARCHITECTURE.md
- Created AGENT-FEATURES.md (technical + sales value props)

## What is nixcfg?

`~/Code/nixcfg` — NixOS/macOS flake managing Markus's personal infrastructure.

| Host | Type | Location | Agents |
|------|------|----------|--------|
| csb0 | Hetzner VPS | Nuremberg | None (smart home backend) |
| csb1 | Hetzner VPS | Nuremberg | None (Grafana, InfluxDB, PPM, was NixFleet) |
| hsb0 | Mac Mini | Home | Merlin + Nimue (OpenClaw multi-agent) |
| hsb1 | Mac Mini | Home | None (Home Assistant, Plex) |
| hsb2 | Raspberry Pi | Home | None |
| hsb8 | Raspberry Pi | Home | None |
| gpc0 | Mac Mini | Home | None (Grandparents) |
| imac0 | iMac | Home | None (workstation) |
| miniserver-bp | Mini PC | BytePoets office | Percy agent |

**PPM**: NIX project (project ID: 1)

### Key difference from dsccfg

nixcfg has many more hosts, uses the `hokage` module system from pbek-nixcfg, and manages home automation (HA, Mosquitto, Zigbee). dsccfg is lean and focused on AI agent deployment.

---

## The AI-CTO Product (business context)

DSC-AI sells AI agents to Austrian/EU SMEs. Three tiers:

| Tier | Infra | Models | Data Residency |
|------|-------|--------|----------------|
| **Local CTO** | Customer Mac Minis + NixOS | exo (local) | 100% on-prem |
| **Cloud CTO** | Hetzner VPS (NixOS) | Claude/GPT/OpenRouter | EU-hosted |
| **Cloud Assistant** | Shared Hetzner VPS | Lighter models | EU-hosted, multi-tenant |

Austria's strict DSGVO + EU AI Act = competitive moat. KMU.DIGITAL 4.0 covers up to €6k subsidy.

### Agent Baseline Spec (every agent ships with)

- Voice input (STT via Groq Whisper) + voice output (TTS — local model planned, see DSC26-51)
- Model selection with fallback chain + `/modelhelp` skill
- Web search + fetch, persistent memory (local GGUF embeddings), workspace sync
- Telegram + Control UI channels
- Heartbeat with cost controls, agenix secrets, container isolation
- Full spec in `~/Code/dsccfg/docs/ARCHITECTURE.md` and `~/Code/dsccfg/docs/AGENT-FEATURES.md`

---

## PPM — How Task Tracking Works

PPM is self-hosted at pm.barta.cm. Auth via `$PPMAPIKEY` bearer token.

### Projects

| Project | ID | Key | Repo |
|---------|------|------|------|
| NixCfg Infrastructure | 1 | NIX | ~/Code/nixcfg |
| DSC Infrastructure | 2 | DSC26 | ~/Code/dsccfg + ~/Code/fleetcom |

### FleetCom tickets live under DSC26-52

Create all FleetCom tickets with `"parent_id": 172` (DSC26-52 epic).

### Workflow rules

- Before starting work: check PPM for backing ticket, create if needed
- Start timer: `POST /api/issues/{id}/time-entries` with `{"user_id": 2}`
- Stop timer: `PUT /api/time-entries/{id}` with `{"stopped_at": "ISO8601"}`
- Log flat hours: `POST /api/issues/{id}/time-entries` with `{"user_id": 2, "override": 1.5, "comment": "..."}`
- When task done: update PPM status immediately
- When updating local files: check if PPM needs a corresponding update

---

## NixFleet (legacy — being replaced by FleetCom)

`~/Code/nixfleet` — Go fleet management dashboard that ran on csb1.

**Being decommissioned** (DSC26-53). The running instance will be disabled. Code stays for reference.

### Patterns to adopt in FleetCom

- **WebSocket hub-and-spoke** — agents connect to dashboard (but FleetCom MVP uses simpler HTTPS POST heartbeats)
- **Compartment indicators** — 5-color status system (green/yellow/red/blue/gray)
- **Op Engine** — state machine for commands (validating → running → success/error)
- **Concurrent heartbeats** — agents report health even during long commands
- **SQLite + Go** — zero external dependencies

### Patterns to NOT adopt

- Gorilla WebSocket complexity (MVP uses simple polling)
- Templ templating (MVP uses single HTML file)
- 20K lines of Go (MVP target: ~1000 lines)

---

## Infrastructure & Access

### Cloudflare DNS (barta.cm)

Markus manages DNS via Cloudflare. Services on barta.cm:
- pm.barta.cm → PPM (csb1)
- fleet.barta.cm → FleetCom (csb1, to be configured)
- fleetcom.barta.cm → redirect to fleet.barta.cm

### Tailscale / Headscale

All hosts are on a Tailscale mesh via Markus's Headscale server at hs.barta.cm. Tailscale IPs are in the 100.64.0.x range. DNS: `<hostname>.ts.barta.cm`.

### SSH everywhere

- Port 2222 (non-standard)
- Key-only auth (`~/.ssh/dsccfg_deploy` for DSC hosts)
- mba user + passwordless sudo (never root SSH)
- `PermitRootLogin = "no"` on all hosts

### Secrets

- **Agenix** for NixOS hosts (age-encrypted, mounted at `/run/agenix/`)
- **1Password** for personal credentials (no agent access)
- **NEVER** read, open, decrypt, or print .env, .age, or secret files

---

## Key Decisions Made in This Session

1. **FleetCom replaces NixFleet** — fresh codebase, inspired by patterns
2. **Go + SQLite + Alpine.js** — modern and reactive but no build step, no npm, no node_modules
3. **SSE for real-time updates** — server pushes to browser instantly on heartbeat, no polling
4. **Heartbeat via HTTPS POST** — shell script agent (cron 60s), not WebSocket
5. **Container-aware + agent-aware** — grid shows per-container and per-agent status, not just 3 flat dots
6. **Security first** — TOTP + password, per-host bearer tokens, Tailscale mesh, all HTTPS, HttpOnly cookies
7. **API-driven** — REST API first, UI is a thin reactive client
8. **Self-hosted Alpine.js** — no CDN runtime dependency, vendored in static/
9. **Hosted on csb1** — same infra as NixFleet was, behind Cloudflare DNS
10. **Local TTS planned** — Kokoro (EN+ES) or Chatterbox Turbo (DE) instead of ElevenLabs (DSC26-51)
