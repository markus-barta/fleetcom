# FleetCom 1.0 — Release Notes

**Released**: 2026-05-03
**Tagline**: Secure-by-default agent-bridge pairing, mission-console wizard UX, audited.

---

## What 1.0 means

FleetCom 1.0 is the first version with:

1. A **stable API contract** documented at `GET /api/info` (FLEET-80) and locked in by drift-protection — every registered chi route must appear in the catalog or `cmd/server/router_test.go` fails the build.
2. A **stable Bosun protocol** — heartbeat schema (`POST /api/heartbeat`), real-time container events (`POST /api/container-events`), agent-observability events (`POST /api/agent-events`), command-result reply channel (`POST /api/command-results`), bridge registration (`POST /api/bridges/register`).
3. A **stable bridge-pairing security model** — N-of-3 trust (host bearer token + operator approval + gateway co-signature), documented in `docs/PAIRING-SECURITY-MODEL.md`, encoded as 3 named postures (`Auto-pair` / `Reviewed` / `Hardened`).
4. A **stable wizard UX** — pair-flow modal with preflight, bridge-deploy modal with command preview + auto-advance, verify-approve modal with side-by-side fingerprint diff, first-run banner with smart routing. Engineering-facing spec in `docs/WIZARD-DESIGN.md`.
5. **Audit-driven hardening** — the FLEET-111 + FLEET-115 surfaces went through a 4-pass post-implementation review (security · logic · UX · code quality). All 12 findings have been addressed in code or filed for follow-up.

The API contract being stable means: existing host bearer tokens, agent-bridge containers, bosun deployments, share-link viewers, and `fleet_pat_*` API tokens continue to work. No data migration. No re-pairing. No re-deployment.

---

## What shipped 0.8.2 → 1.0.0

**13 versions in one day (2026-05-03)**, broken into 6 epics:

| Epic | Tickets | Versions | What |
|------|---------|----------|------|
| **MVP-183** | FLEET-79/80/81 | v0.8.3 | User-issued read-only `fleet_pat_` API tokens, `/api/info` self-describing catalog, `AGENTS.md` cross-tool onboarding doc |
| **Drawer Phase 2** | FLEET-91/92/93/199/369.1/377/98 | v0.8.4 | Inline drawer-driven UX (no more Settings tabs for Gateways/Bridges); `host.reboot` via D-Bus; pair-status flips immediately |
| **UX foundations (FLEET-103)** | FLEET-104..109 | v0.8.5 | Modal-inside-x-data fix; Toast v2 (4 levels, dedupe, sticky errors); `busy()` helper; `confirmModal()` (banned native `confirm()`); operator activity log; bridge-deploy chip rails |
| **Secure pairing (FLEET-111)** | FLEET-112/113/114 + QA | v0.9.0..v0.9.3 | Pending-pair approval surface; OOB confirmation code (Signal-style salted hash); Ed25519 attestation chain |
| **Quick-win UX** | FLEET-116 | v0.9.4 | Plain-language tooltips + admin-only "What is this?" 3-layer security explainer |
| **Operator UX wizard (FLEET-115)** | FLEET-117/118/119/120/121 | v0.9.5..v0.9.9 | Posture cards + factor stack; pair-flow modal with preflight; deploy phase machine with auto-advance; verify-approve modal with fingerprint diff + TOFU; first-run banner + smart routing + help-drawer integration |
| **QA hardening** | (post-FLEET-115 audit) | v0.9.10..v0.9.11 | Atomic OOB rate-limit (TOCTOU fix); deploy-phase mislabel; TOFU chevron sync; symmetric auto+OOB guard; banner sig + fp; debounced loadOnboarding; pairFlow timeout; defensive fallbacks |
| **1.0 milestone** | — | v1.0.0 | This release |

---

## Stable surfaces

### Public API (no auth)
- `GET /api/info` — self-describing catalog (auth methods, endpoints, scope mapping, links)
- `GET /api/version` — build version + commit + feature flags
- `GET /api/settings` — public settings (heartbeat interval, branding, instance domain)
- `GET /api/org-logo` — org logo PNG
- `GET /s/{token}` + `GET /s/{token}/events` — share-link read-only dashboard

### Programmatic API (`fleet_pat_*` tokens, FLEET-79)
- `GET /api/hosts` (scope `read:hosts`)
- `GET /api/hosts/{hostname}/hardware` (scope `read:hardware`)
- `GET /api/agents` (scope `read:agents`)
- `GET /api/agents/{host}/{name}` (scope `read:agents`)

Tokens are issued from the dashboard (Settings → Account → API Tokens) with operator-chosen scopes and expiry. Owner-revocable. SHA-256-hashed at rest. Inherit owner's `user_host_access` rows. Audit-logged on creation/revocation/auth-failure. Rate-limited (10/10min per IP+prefix).

### Bosun protocol (per-host bearer token)
- `POST /api/heartbeat` — periodic enriched (60s; hosts, containers, agents, hw, deployment_shape, boot_id)
- `POST /api/container-events` — real-time Docker socket events (die, restart, oom, health_status)
- `POST /api/agent-events` — per-turn agent observability (turn started, first token, tool invocations, errors)
- `POST /api/bridges/register` — agent-bridge announces itself + pubkey
- `POST /api/command-results` — bosun reports back the result of pulled commands

### Browser session (cookie + mandatory TOTP)
- All admin operations (pair gateway, set posture, approve bridges, manage users, host operations)
- Mandatory TOTP enforced on first login
- HttpOnly cookies, 24h, SameSite=Lax, Secure
- Per-user `user_host_access` filtering (admins bypass)

### Bridge-pairing security model
- **3 named postures** (`Auto-pair` / `Reviewed` / `Hardened`) collapse the underlying flag triple into one decision per gateway
- **N-of-3 trust**: host bearer token (always on) + operator approval (auto-approve OFF) + gateway co-signature (Ed25519 attestation + Signal-style OOB code)
- **Atomic posture endpoint**: `POST /api/gateways/{host}/posture/{name}` — one UPDATE, no intermediate state
- **Hardened gated** until gateway pubkey is captured (operator paste via `PUT /api/gateways/{host}/pubkey`); future: auto-capture during pair time when OpenClaw RFC ships
- **OOB rate limit**: 5 attempts per pending row, atomically enforced via `WHERE confirmation_attempts < ?` (audit-fix v0.9.10)
- **Audit row** for every state-changing operation; `OOB_BYPASSED` written server-side regardless of UI/curl origin
- **Fingerprint pinning**: TOFU on first approval; re-register with different fp lands as a new pending row, never silently overwrites

Threat-model details: `docs/PAIRING-SECURITY-MODEL.md`.

### Operator UX wizard
- **Posture cards** (FLEET-117) — 3 cards with the "factor stack" visual primitive (3× 24×6px segments). One click, one decision per gateway. Original 4-toggle row collapsed into Advanced disclosure.
- **Pair-flow modal** (FLEET-118) — preflight checklist (bosun freshness + TCP dial + TLS handshake to wss://host:18789 with 3s timeout) before clicking Pair; launch-sequence log during pairing.
- **Bridge-deploy modal** (FLEET-119) — chip rails for agent picker; reactive `docker run` command preview; auto-advance on SSE pair-request arrival; 90s watchdog.
- **Verify-approve modal** (FLEET-120) — side-by-side fingerprint diff; copy verify command; TOFU disclosure; OOB code input when enforced.
- **First-run banner + smart routing** (FLEET-121) — admin-only band above the host grid; "Start setup →" routes to the most actionable step (pending → deploy → pair fallback chain).

Aesthetic spec: `docs/WIZARD-DESIGN.md` — "mission console" (Apollo control × Unix build log).

---

## Deployments

### Personal — fleet.barta.cm
- **Host**: csb1 (Hetzner VPS, NixOS)
- **Image**: `ghcr.io/markus-barta/fleetcom:1.0.0`
- **Auto-deploy**: push to main → CI → SSH + `/etc/fleetcom-deploy.sh`

### BYTEPOETS — fleet.bytepoets.com
- **Host**: Hetzner VPS at 5.75.130.206 (Ubuntu, shared with PMO)
- **Manual deploy**: trigger "Deploy to BYTEPOETS" CI workflow

Both deployments share the same image; per-tenant configuration via env file.

---

## Tested + audited

- **35 backend tests**, all green:
  - `bridges_test.go` (10) — security primitives (OOB code generation, salted hash, attestation verify, env parsing)
  - `bridge_pairings_test.go` (8) — DB-layer (posture mapping, locked-without-pubkey, atomic-cap-enforced confirmation attempts)
  - `gateway_preflight_test.go` (5) — preflight blockers (unknown host, never-seen-bosun, stale heartbeat, fresh row, JSON shape)
  - `onboarding_test.go` (9) — onboarding state buckets + sort order + JSON shape
  - `posture_guard_test.go` (3) — auto+OOB symmetric 422 guard
- **Drift protection** (FLEET-80) — every registered chi route appears in `info.go` catalog
- **CI lint gate**: `gofmt -l .` + `go vet ./...` + `staticcheck ./...`
- **4-pass QA audit** (security · logic · UX · code quality) post-FLEET-115:
  - 1× HIGH-MEDIUM (OOB rate-limit TOCTOU) — fixed v0.9.10
  - 3× MEDIUM (deploy-phase mislabel, TOFU chevron desync, dead native-prompt code) — fixed v0.9.10
  - 8× LOW (silent swallows, banner sig granularity, triple-fetch debounce, etc.) — fixed v0.9.11

---

## Known deferred work

These are explicit "we knew, we deferred" — not bugs.

### Hardened posture gating
The `Hardened` posture card is **locked** in the UI until two things ship on the gateway side:
1. **`/v1/bridge/sign-registration`** — gateway-side Ed25519 signing of (host:agent:fp) tuple. Until this lands, attestation always falls through to `attestation_status='skipped'` with a system audit row. Server-side machinery is fully shipped (FLEET-114, v0.9.0); the bridge container fetches via `BRIDGE_GATEWAY_SIGN_URL` env when set (no-op otherwise).
2. **`bridge.confirmation_code` WS RPC** — gateway-side delivery of the 6-digit OOB code through the agent itself (e.g. Ocean DMs you on Telegram). Until this lands, OOB is dormant and operators must use SKIP OOB on every approval. Server-side fully shipped (FLEET-113, v0.9.0).

When the OpenClaw RFC ships these:
1. Upgrade gateway containers to a version that signs registrations + emits OOB codes
2. Paste the gateway pubkey via the `+ pubkey` button (or wait for FLEET-117 to auto-capture during MarkGatewayPaired)
3. Switch posture from `Reviewed` → `Hardened` (one click in the cards)
4. Set `FLEETCOM_REGISTER_ATTESTATION_REQUIRED=true` in the FleetCom server env
5. Every new bridge registration now requires N-of-3 compromise to spoof

### Multi-instance / multi-tenant
The `BYTEPOETS` instance shares the codebase with the personal `fleet.barta.cm` instance — different env, different DB, same binary. There is no per-tenant isolation in the binary itself; tenant separation is achieved by running separate processes with separate DBs.

### Attestation env default
`FLEETCOM_REGISTER_ATTESTATION_REQUIRED` defaults to **FALSE** for the v1 ship. The end-state per FLEET-114 spec is default TRUE; flip the default once a fleet's audit shows zero `ATTESTATION_SKIPPED` rows for ≥2 weeks across all gateways. The env stays in place to let individual operators upgrade earlier.

---

## Aesthetic

The dashboard is **operations console / telemetry grid** — dark, monospace where it earns its place, sharp corners, status flags in fixed brackets (`[OK]` / `[…]` / `[!!]`). The wizard surfaces (FLEET-115 epic) commit harder to this with **mission console** (Apollo control × Unix build log) — see `docs/WIZARD-DESIGN.md`.

The visual signature is the **factor stack** — a horizontal trio of 38×8px (or 24×6px in the cards) segments that fills as N-of-3 trust factors are added. It appears in the posture card (its main affordance), and is the design hook for FLEET-121's first-run banner and the (deferred) docked-resume chip.

---

## See also

- `docs/PAIRING-SECURITY-MODEL.md` — engineering-facing canonical threat model
- `docs/WIZARD-DESIGN.md` — visual & interaction spec for FLEET-115
- `docs/DEPLOYMENT.md` — how to ship a release, version-bump rules, runbooks per host
- `docs/ARCHITECTURE.md` — system layout, data flow, why-we-chose
- `AGENTS.md` — cross-tool agent onboarding (also reachable as `CLAUDE.md` symlink)
- `GET https://fleet.barta.cm/api/info` — the canonical, drift-protected API surface

---

## Acknowledgements

This release stitched together work from operators, multiple Claude sessions across multiple days, and a tight feedback loop with the FleetCom owner. PPM (project management) tracked every ticket, every time entry, every commit reference — see project FleetCom at `https://pm.barta.cm` for the full history.

Built with Go (`stdlib net/http` + `chi` router + `modernc.org/sqlite`), Alpine.js 3, self-hosted Lucide, and zero npm. Single-HTML-file frontend, no build step. Pure-Go SQLite, no CGO. Bosun + agent-bridge in Docker via watchtower (or systemd-native via the FLEET-83/87/88 universal updater).

---

**v1.0.0 · 2026-05-03**
