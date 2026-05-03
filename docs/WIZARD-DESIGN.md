# FleetCom Pairing Wizard — Design Spec

The visual & interaction spec for **FLEET-115** (operator-facing pairing wizard) and its 5 children (FLEET-117..121). Engineering-facing canonical source — the operator-facing surface mirrors this content.

> Cold-start TL;DR for engineers picking this up: the wizard collapses FLEET-111's 4-toggle security-primitives row into a guided 5-step flow with 3 named postures. The aesthetic is **mission console** — Apollo control panel × Unix build log. One distinctive visual primitive (the *factor stack*) is reused across the posture card, the docked-resume chip, and the first-run banner so the wizard reads as one piece.

## Aesthetic commitment

**Mission console** — Apollo mission control crossed with a Unix build log.

- Sharp corners, zero border-radius on wizard surfaces
- JetBrains Mono everywhere it earns its place (status flags, fingerprints, timestamps, command previews)
- Status flags in fixed brackets: `[OK] […] [--] [!!]` borrowed from `make` output
- Real timestamps, fixed-width, monospace
- Tight line-heights (1.3–1.6) — operational text, not editorial
- Zero soft drop-shadows; zero purple anything; no rounded "wizard cards"
- One atmospheric quirk: a 1px-wide CRT-scanline overlay at `rgba(255,255,255,0.012)` drifting at 0.5Hz — present but barely perceptible

**The visual hook**: the *factor stack* — a horizontal trio of segmented bars (3× 38×8px) that fills as N-of-3 trust factors are added. Reused across the posture card (its main affordance), the docked-resume chip (progress meter), and the first-run banner (mini indicator). Same primitive, three appearances. This unifies the wizard as one design.

**What this is not** — generic AI wizard tropes:
- No Stripe-style horizontal stepper at top
- No Material `<progress>` bar for SSE
- No rounded purple cards
- No big "Welcome to FleetCom!" splash

## Design decisions

### Wizard shell — full-page take-over

A side drawer is too cramped (operators need to read SSE logs + paste fingerprints side-by-side). A modal feels temporary; this is a 5-step journey that may pause for an SSH tab. The wizard is its own surface with a `← FleetCom` link top-left always visible.

*Tradeoff:* operators lose live dashboard context while wizarding — accepted because (a) the wizard runs at most once per host, and (b) step panels carry their own host-scoped SSE feeds.

### Step indicator — vertical left-rail (240px)

Horizontal steppers force step labels into narrow widths and look like every Stripe wizard ever. The vertical rail can carry rich rows that double as live summary:

```
[OK]  01 / posture        Reviewed
[OK]  02 / pair gateway   dsc0 ✓
[…]   03 / deploy bridge  merlin
[--]  04 / approve        —
[--]  05 / verify         —
```

Status-flag conventions:
- `[OK]` — completed (`--ok` green)
- `[…]` — in progress, current step (`--ink` cyan)
- `[!!]` — error needing attention (`--crit` red)
- `[--]` — not yet (`--ink-dim` grey)
- `[~~]` — locked / will skip (`--ink-faint`)

*Tradeoff:* 240px of real estate; on screens <1024px the rail collapses to 48px (flag column only).

### Posture cards (FLEET-117) — factor stack + LOCKED that is never silent

Three cards, horizontal grid. Each carries:

- **Header**: name in monospace caps + recommendation chip (Reviewed) or `[LOCKED]` chip (Hardened pre-RFC)
- **Factor stack**: `[█][█][░]` — 3 horizontal 38×8px segments showing which trust factors are active
- **Body**: 1-line description in `--ink-dim`
- **Footer**: per-posture "when to use" line in `--ink-ghost`, separated by a 1px dashed border

| Posture     | Factors | Stack visual | Use when |
|-------------|---------|--------------|----------|
| Auto-pair   | 1/3     | `[█][░][░]`  | Lab/dev. Restricted token distribution. |
| Reviewed *(default)* | 2/3 | `[█][█][░]` | Production today. Attestation column staged. |
| Hardened *(locked)*  | 3/3 | `[█][█][╳]` (dashed third) | Once OpenClaw RFC ships + gateway pubkey pasted. |

**Locked-card behavior**: clickable but click triggers an inline tooltip ("Requires gateway pubkey + OpenClaw RFC v0.X") rather than a silent dead-button click. This avoids the "I clicked and nothing happened" trap that exposed FLEET-111's 4-toggle confusion.

The selected card gets a 1px `--accent` border + an inset glow (`box-shadow:inset 0 0 0 1px var(--accent)`). The recommended card (Reviewed) gets a small `RECOMMENDED` chip in the header.

**Below the cards**: a small `Advanced toggles ▸` disclosure that expands to show the existing 4-toggle row. Power users can still flip individual toggles for non-canonical combinations (which render in the posture cards as `Custom` — a neutral 4th badge).

*Tradeoff:* hover-only lock-reason explanation is desktop-biased; mobile gets a permanent caption beneath the stack.

### SSE-streamed progress (FLEET-118) — launch-sequence log

A horizontal `<progress>` bar tells operators *that* something is happening but not *what*. For a security-sensitive flow that's not enough.

The log surface uses fixed-width timestamps + status-flag flips: a row appears with `[…]` and start timestamp, then on the SSE event the same row's flag flips to `[OK]` with a separate "elapsed" timestamp. Same DOM row, status-class change, CSS transition on the flag color (200ms ease).

```
2026-05-03 16:23:01.247  […]   Verifying host token (dsc0)
2026-05-03 16:23:01.413  [OK]  Verifying host token (dsc0)            166ms
2026-05-03 16:23:01.413  […]   Probing bosun on dsc0
2026-05-03 16:23:01.892  [OK]  Probing bosun on dsc0                  479ms
2026-05-03 16:23:01.893  […]   Enqueueing openclaw.pair
2026-05-03 16:23:02.104  ↓     SSE bosun.command-result
2026-05-03 16:23:02.105  [OK]  Enqueueing openclaw.pair               212ms
2026-05-03 16:23:02.106  […]   Awaiting gateway ack
2026-05-03 16:23:02.108  ↓     SSE gateway.paired
2026-05-03 16:23:02.108  [OK]  Awaiting gateway ack                   2ms
─────────────────────────────────────────────────────────────────
[OK]  All preflight checks complete                                   861ms
```

**Conventions**:
- Local checks render with status flags (`[OK]`, `[…]`, `[!!]`)
- SSE-arrival rows render with a `↓` glyph so the operator sees what came from the wire
- Errors get a `--crit` left-border + the row stays highlighted + a "Retry" button appears at the bottom
- Pending rows have a 1Hz blinking `[…]` (CSS animation)

*Tradeoff:* more code than `<progress>`, but earns trust by being explicit. Justified for a security-flow.

### Bridge-deploy step (FLEET-119) — chip rails + command preview

Two-pane layout inside `.wiz-pane`:

**Left pane (40%)** — Agent picker:
1. *Existing bridges*: read-only chip list, grayed out
2. *Suggested next agent*: chip rails from `GET /api/bridges/suggestions/{host}` (FLEET-109), primary color
3. *Custom*: free-text input + "+ Add" button

**Right pane (60%)** — Live command preview:
- A shell-prompt-style block (reuses the `.seq` styling from FLEET-118):
  ```
  $ ssh dsc0 'cd /home/mba/docker && docker run -d \
      --name fleetcom-bridge-merlin \
      --network host \
      -e FLEETCOM_URL=https://fleet.barta.cm \
      -e BRIDGE_GATEWAY_URL=ws://localhost:8090 \
      -e BRIDGE_AGENT=merlin \
      -e BRIDGE_TOKEN=$BOSUN_TOKEN \
      ghcr.io/markus-barta/fleetcom-bridge:0.6.0'
  ```
- Syntax highlighting: env-var names in `--ink`, paths in `--ink-dim`, image in `--accent`
- Two buttons: `[Copy command]` (ghost) + `[Deploy via FleetCom]` (primary)
- Below: live SSE feed pane that boots when Deploy is clicked

**Auto-advance**: when SSE delivers `bridge.pair.requested` matching the chosen agent, wizard auto-advances to the approval step with a brief `→ Approve fingerprint` toast.

### Approval step (FLEET-120) — fingerprint diff + TOFU explainer

The wizard's hero moment. Two columns side-by-side:

```
┌─ FleetCom sees ─────────────┐  ┌─ dsc0/merlin reports ────────┐
│ Agent     merlin            │  │ Agent     merlin             │  ✓ MATCH
│ Host      dsc0              │  │ Host      dsc0               │  ✓ MATCH
│ Pubkey    Y2lwaGVydGV4...   │  │ Pubkey    Y2lwaGVydGV4...    │  ✓ MATCH
│ FP        a3:f1:9c:7d       │  │ FP        a3:f1:9c:7d        │  ✓ MATCH
│           :4e:8b:2a:1f      │  │           :4e:8b:2a:1f       │  ✓ MATCH
└─────────────────────────────┘  └──────────────────────────────┘
                ✓ FINGERPRINTS MATCH — APPROVE ENABLED
```

**Conventions**:
- Per-row chip: `MATCH ✓` (`--ok`) / `MISMATCH ✗` (`--crit` + line-through on the value)
- All rows match → thin `--ok` border on both panels + a footer banner `✓ FINGERPRINTS MATCH — APPROVE ENABLED`
- Any row mismatches → `--crit` border on the right panel, matching rows still show ✓ (don't fudge), the APPROVE button is replaced by `[!!] Mismatch detected`
- Right column starts empty with a "How to populate" disclosure:
  ```
  Run on dsc0 (over SSH):
    $ docker exec fleetcom-bridge-merlin bridge-fp
  Then paste the JSON output below.
  ```
  + textarea + "Verify" button. Once pasted, the right column populates and the diff renders.

**TOFU explainer** (below the diff, in a `--bg-1` card):
> **TOFU = Trust On First Use.** By approving this fingerprint, you're telling FleetCom: *"this is the real merlin."* From now on, any bridge claiming to be merlin must present this exact fingerprint, or it'll be rejected. If the fingerprint changes (legitimately — e.g. you re-deployed the bridge), you'll see a new pending pair-request and need to approve again.

*Tradeoff:* more visual machinery than a single-line "they match!" — but this is the moment the operator is being asked to trust crypto material; the UI should be extremely deliberate.

### Wizard chrome (FLEET-121)

**First-run banner**:
- Triggered by `unpaired_gateways.length > 0 OR pending_bridges.length > 0`
- Renders as a 32px-tall thin band at the top of the dashboard (above the existing header), `--bg-1` background, 2px `--warn` left-border
- Single line: `[!] dsc0 has unpaired gateways · Run pairing checklist →`
- Per-host dismissible via `localStorage` key `pair-banner-dismiss-{host}`
- Auto-clears on completion

**Resumability**:
- When operator clicks `[—]` in the wizard, it docks as a 32px-tall pill in the header (slot before user-chip): `02/05 · pair dsc0 ▒▒▒▒░░` — the factor stack reused as progress visualization
- Click expands back to full wizard
- State persisted via `GET/PUT /api/onboarding/state` (`{user_id, host, step, posture_choice, last_updated_at}`)

**Help-drawer integration**:
- Each step has a `(?) Why this step` link in the top-right of the panel
- Click opens FLEET-116's "What is this?" modal scoped to that step's section

## PPM mapping

| Ticket | Visual primitive | Notes |
|--------|------------------|-------|
| **FLEET-117** | Posture cards + factor stack | The conceptual anchor; ships first as the MVP win |
| **FLEET-118** | Launch-sequence log | Reusable `.seq` primitive — also used by FLEET-119's command preview |
| **FLEET-119** | Two-pane chip-rails + command preview | Reuses FLEET-109 chip primitives; auto-advance on SSE pair-request |
| **FLEET-120** | Fingerprint diff + TOFU explainer | The hero moment; per-row chips + global banner |
| **FLEET-121** | First-run banner + minimize chip | Reuses FLEET-117's posture-stack as progress meter |

## File references (when implementing)

| File | What |
|------|------|
| `backend/internal/api/bridges.go` | Add `SetGatewayPosture` handler (FLEET-117); reuse `SetGatewayAutoApprove` / `SetGatewayOOBDelivery` / `SetGatewayAttestationRequired` semantics in one atomic call |
| `backend/internal/db/bridge_pairings.go` | Add `SetGatewayPosture(host, posture string) error` helper |
| `backend/internal/api/info.go` | Catalog new endpoints (drift-protection enforced by `cmd/server/router_test.go`) |
| `backend/internal/api/preflight.go` | NEW — `GET /api/gateways/{host}/preflight` (FLEET-118) |
| `backend/internal/api/onboarding.go` | NEW — `GET/PUT /api/onboarding/state` (FLEET-121) |
| `backend/static/index.html` | All wizard surfaces; reuse `confirmModal()`, `busy()`, `Toast v2`, `recordOplog()` (already shipped in FLEET-103 epic) |

## Naming pending

Posture names: **Auto-pair / Reviewed / Hardened**. Pending operator confirmation. Alternatives the operator asked about:
- Open / Standard / Strict
- Casual / Recommended / Maximum

## See also

- **FLEET-111** epic — security primitives the wizard layers on top of
- **FLEET-115** epic — operator-UX wizard (this doc's reason to exist)
- **FLEET-116** — quick-win UX hardening (shipped v0.9.4): tooltip rewrites + "What is this?" drawer; the modal copy is mirrored from `docs/PAIRING-SECURITY-MODEL.md`
- `docs/PAIRING-SECURITY-MODEL.md` — engineering-facing threat model the wizard's posture cards encode
- `docs/AGENT-BRIDGE-PAIRING.md` — original (pre-FLEET-111) flow; superseded for security model but accurate for bridge-side reference
