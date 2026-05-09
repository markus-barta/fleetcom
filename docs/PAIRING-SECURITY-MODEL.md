# Bridge-Pairing Security Model

The threat model and operator decision tree behind the OpenClaw integration shipped in **FLEET-111** (epic, v0.9.x). This is the engineering-facing canonical source — the operator-facing copy used in tooltips, the FLEET-116 info drawer, and the FLEET-117 wizard mirrors this content.

> Cold-start TL;DR for engineers picking this up: the integration is a layered N-of-3 trust model. Each layer closes one specific class of attack. Operators don't need to understand the layers in isolation — they pick a *posture* (Auto-pair / Reviewed / Hardened) and the layers compose accordingly. The wizard surface is FLEET-115; the primitives below are FLEET-111.

## What this protects

A **bridge** is a small program that runs on a managed host alongside an AI agent (Ocean, Merlin, etc.). Its job is to translate per-turn agent activity into FleetCom dashboard events. To do that it needs FleetCom's permission — and FleetCom needs to know the bridge is what it claims to be.

The hard question: *"how does FleetCom know this `Ocean` bridge is the real Ocean and not someone with a stolen token claiming to be Ocean?"*

The answer is N-of-3: an attacker must break **all three** of the layers below to plant a fake bridge. Breaking any one or two leaves the others as a backstop.

## Layer 0 — transport (TLS): confidentiality, not authentication

The operator-session WebSocket from FleetCom to each OpenClaw gateway runs over `wss://`. TLS here is intentionally configured with `InsecureSkipVerify=true` (`backend/internal/openclaw/client.go::operatorTLSClient`). FleetCom does **not** validate the gateway's certificate chain or hostname.

That is a deliberate split between transport and identity:

- **TLS gives us:** confidentiality against passive sniffing on the wire and integrity (MAC) against an in-path attacker tampering with bytes.
- **TLS does NOT give us:** authentication of the gateway. That is the job of the `connect.challenge` handshake — the gateway must sign FleetCom's nonce with its Ed25519 private key, verified against the pinned `gateway_pubkey_b64` (auto-pinned via TOFU on first connect, FLEET-123). An MITM who can substitute a TLS cert still cannot forge that signature without the gateway's private key.

Why not validate the cert? Three options were considered (FLEET-125):

1. **Issue gateway certs with matching SANs** (Tailscale MagicDNS, in-cluster CA, or per-host self-signed). Adds permanent PKI ops on every gateway host — generation, rotation, distribution — to enforce a check that the spec does not name as the trust anchor. Cost without security delta.
2. **Pin the cert fingerprint via TLS-layer TOFU.** Adds a second TOFU store running parallel to the existing Ed25519 pubkey TOFU. Both pin the same peer; they can drift apart on legitimate rotation and produce confusing failure modes. No incremental security against an attacker who would already need the Ed25519 private key to complete the handshake.
3. **`InsecureSkipVerify` and rely on the Ed25519 challenge.** *(chosen)* Aligns transport with the stated trust model: identity is application-layer, transport is encrypted-but-anonymous. One trust factor, one source of truth.

Going to option (1) or (2) would only strengthen the model if the Ed25519 trust factor itself were broken — at which point N-of-3 has already collapsed. The design floor for this system is N=3 (host token + operator + gateway co-sign); a hypothetical N=4 via TLS validation would not change any cell of the threat-model table at the bottom of this doc.

## The bridge ↔ gateway connection

The N-of-3 layers below describe the **bridge → FleetCom** trust chain. There is a second, independent authentication chain — the **bridge → its local OpenClaw gateway** WebSocket — that took until FLEET-127 / FLEET-134 to land cleanly and was historically undocumented. It's worth understanding because each of the layers below depends on the bridge actually being able to talk to its gateway in the first place.

### What auth path the bridge uses today (FLEET-134)

OpenClaw 2026.4.x's connect-policy short-circuits the device-identity (paired-pubkey) check when:

```js
roleCanSkipDeviceIdentity(role, sharedAuthOk) {
    return role === "operator" && sharedAuthOk;
}
```

i.e. *role=`operator`* AND *`auth.token` matches the gateway's configured `gateway.auth.token` shared secret*. The bridge claims `role: "operator"`, so the only thing that gates its connection is whether it can present the gateway's shared secret.

That secret is the file the gateway already mounts at `/run/secrets/gateway-token` (sourced from `/run/agenix/<host>-ocean-gateway-token` at provisioning time). Bosun (FLEET-134) inspects the gateway container, finds the host-side path of that mount, and **mirrors the same bind-mount into the bridge container at the same destination**. Bosun never reads the file's contents; the bridge reads it on startup and sends it as `auth.token` in the connect frame.

### Tradeoff

Shared-secret auth grants the bridge the full operator scope on the gateway. A compromised bridge container has the same gateway-side capability as a compromised operator session. Mitigating factors:

- **Co-located blast radius.** The bridge runs on the same host as the gateway. A compromise of one is roughly equivalent to a compromise of the other; isolating bridge-vs-gateway within one host doesn't add meaningful defense.
- **Read-only intent.** The bridge requests a single scope, `operator.read`. Even though the shared-token path skips scope enforcement, the bridge code never makes write calls. A bridge that started writing would surface immediately in the audit-friendly gateway logs.
- **Operator-asserted lifecycle.** Bridge install, reinstall, and remove all flow through bosun's command queue (FLEET-131); the gateway secret is never copied through FleetCom or the WAN.

### Why not bootstrap-tokens

The cleaner long-term model is OpenClaw's bootstrap-token flow (`auth.bootstrapToken` → server-issued `deviceToken` returned in `hello-ok.auth.deviceToken`). With that, each bridge would have its own paired identity in the gateway's `paired.json`, scoped to `operator.read` only, revocable per-bridge.

OpenClaw exposes bootstrap-token issuance only via the `/pair` chat command (`extensions/device-pair/index.js`) or the plugin SDK — not as a callable WS RPC. Automating it from FleetCom requires writing an OpenClaw plugin that exposes `issueDeviceBootstrapToken` over the protocol. That's a bigger lift; tracked separately as a future-improvement candidate.

## Agent-name discipline (FLEET-149)

Independent of cryptographic layers below, the bridge's `agent_names` list — which dictates what `(host, agent)` rows it creates in `bridge_pairings` — is **operator-asserted**, not server-derived. Three rules keep this from drifting:

1. **No defaults anywhere.** Bosun's `bridge.install` rejects an empty `agent_names` param (no fallback to `merlin,nimue` or any other guess). The bridge container itself refuses to start with an empty `BRIDGE_AGENT_NAMES` env. The sample compose uses `${BRIDGE_AGENT_NAMES:?}` so a misconfigured operator hits the failure at compose-up time, not at register time.
2. **Reinstall preserves identity.** `bridge.reinstall` (FLEET-131) inspects the running container's existing `BRIDGE_AGENT_NAMES` env BEFORE tearing it down, and passes it through to the new install. A re-install can never silently change which agents the host claims to run.
3. **Future enforcement (FLEET-149).** The server will validate operator-asserted agent_names against the heartbeat-reported `agents` list for that host, refusing or warning loudly on mismatch. Cross-host name collisions surface a banner. Until that lands, mismatch is caught only by operator eyeballing the dashboard — which is what triggered the ticket.

The visible failure mode this prevents: a misconfigured (or hostile) bridge install registering itself as agents that don't belong on the host, polluting the dashboard with bogus pending pair-requests and mixing per-agent telemetry across hosts.

## The three layers

### Layer 1 — host bearer token *(always on, no toggle)*

Every bridge presents the host's bosun bearer token in `Authorization: Bearer …` on its `POST /api/bridges/register` call. The server hashes the token and looks it up in the `host_tokens` table.

- **Closes:** random attackers on the internet posting fake bridge registrations
- **Doesn't close:** anyone who possesses the token (leaked `.env`, hijacked host process, supply-chain compromise of the bridge image) — they can register any bridge under any agent name

This is the floor. There's no way to disable it, and no plan to add a fourth layer below it.

### Layer 2 — operator approval *(`auto-approve` toggle)*

When `auto_approve_bridges = OFF` (the recommended default for new gateways since FLEET-112), every new bridge registration lands in `bridge_pairings` with `status='pending'`. The dashboard's host drawer shows a `🟡 N PAIR REQUEST · REVIEW` affordance. The operator visually verifies the SSH-style 8-byte fingerprint (`a3:f1:9c:7d:4e:8b:2a:1f`) and clicks APPROVE.

When `auto_approve_bridges = ON`, the FleetCom-side OpenClaw client (in `internal/openclaw/manager.go::handlePairRequested`) auto-approves any registration whose fingerprint matches a known `bridge_pairings.pubkey_fp`.

| auto-approve | Behavior | Cost / benefit |
|---|---|---|
| OFF *(recommended)* | Each bridge sits pending until the operator clicks APPROVE | One factor of human attention per bridge — closes the leaked-token attack |
| ON | FleetCom auto-approves once the host token checks out | Zero friction, but the host token alone is sufficient to register anything |

**Closes (when OFF):** a leaked host token. The attacker can post a registration but can't actually pair until the operator approves it, and the fingerprint they'd see is the attacker's, not the legitimate bridge's — they'd reject.

**Doesn't close (when OFF):** an attacker who has compromised both the host token AND social-engineered the operator into clicking APPROVE on a fake fingerprint.

### Layer 3 — gateway co-signature

This layer is two-part: a cryptographic factor (`attest`) and a human-channel factor (`OOB code`). Either alone is useful; together they implement the third leg of N-of-3.

#### 3a. Cryptographic gateway endorsement *(`attest` toggle + global env + gateway pubkey)*

Before a bridge can register, it asks its local OpenClaw gateway to sign a statement: *"yes, this is the bridge claiming agent `Ocean` with pubkey fingerprint `<fp>` on host `<host>`."* The signature is `Ed25519(gateway_priv, sha256(host || ":" || agent || ":" || fp))`.

The bridge submits the signature in the `gateway_signature_b64` field of the register request. FleetCom verifies it using the gateway's public key stored in `openclaw_gateways.gateway_pubkey_b64`.

Effective enforcement: `enforce = env(FLEETCOM_REGISTER_ATTESTATION_REQUIRED) AND gateway.attestation_required`. Both must be ON. Either OFF means the row is recorded with `attestation_status='skipped'` and a system-level `ATTESTATION_SKIPPED` audit row is written via `RecordActivity` for later operator review.

| Outcome | When |
|---|---|
| `verified` | enforce=true OR enforce=false but a valid signature was provided. Cryptographically endorsed. |
| `skipped` | enforce=false AND signature missing/invalid (or pubkey unknown). Audit row written. |
| `unknown` | Pre-FLEET-114 row. Refreshes to `verified` or `skipped` on next bridge re-registration. |

**Closes:** a leaked host token alone. The attacker would need to also compromise the gateway's signing key, which is on a different host (one of the dsc/csb/hsb fleet) than the bosun token.

**Status today:** FleetCom side fully shipped (FLEET-114, v0.9.0). Gateway side pending: OpenClaw needs to expose `/v1/bridge/sign-registration` (separate RFC, separate repo). Until then, env defaults FALSE, attestation always falls through to `skipped`. The bridge container does try to fetch a signature from `BRIDGE_GATEWAY_SIGN_URL` if set — currently a no-op.

#### 3b. Out-of-band confirmation code *(`OOB code` toggle)*

When `oob_delivery_enabled = ON` for a gateway, the server mints a 6-digit code on every bridge registration. The hash is `sha256(code || ":" || pubkey_fp)` — salted with the fingerprint so a leaked code cannot approve a different bridge (Signal safety-number model).

The plaintext is pushed to the gateway over its operator WebSocket (`Manager.PushConfirmationCode`). The gateway is supposed to deliver the code through the agent itself — e.g. Ocean DMs the operator on Telegram: *"FleetCom pair confirmation: 472 819. Show this to your operator within 5 minutes."*

The operator reads the code from the agent's user channel and types it into FleetCom's approve endpoint. `subtle.ConstantTimeCompare` on the salted hash. 5-minute TTL. 5-attempt rate limit per pair request, then auto-reject (delete row, bridge must re-register).

The escape hatch is `POST /api/bridges/{host}/{agent}/approve-skip-oob` with a typed-confirm body (`{"confirm":"<hostname>"}`). Server-side audit row `OOB_BYPASSED` is written by the handler regardless of whether the call came from the dashboard or curl.

**Closes:** the "compromised host token AND compromised gateway" attack. The attacker still doesn't control the agent's user channel — Ocean's actual Telegram conversation with David. The operator would see a code arrive (or not) on a channel the attacker can't tamper with.

**Status today:** FleetCom side fully shipped (FLEET-113, v0.9.0). Gateway side pending: OpenClaw needs the `bridge.confirmation_code` WS RPC handler. Until then, OOB is dormant — the server still mints codes but no delivery happens, and operators must use SKIP OOB on every approval.

### How the gateway pubkey lands in `openclaw_gateways.gateway_pubkey_b64`

**Trust-on-first-use auto-pin (FLEET-123).** After the operator-session connect handshake completes, FleetCom calls `gateway.identity.get` over the same WebSocket and stores the returned `publicKey` only when the column is currently empty (`SetGatewayPubkeyTOFU`). The captured key is bound to the just-authenticated peer because the gateway already proved possession of the matching Ed25519 private key by signing FleetCom's `connect.challenge` nonce in the handshake immediately preceding this call. Every subsequent reconnect re-runs the call as a continuity check — a divergent key logs a warning and refuses to overwrite.

**Manual paste is the fallback.** The host-drawer `+ pubkey` chip opens a paste form for legitimate rotation, recovery, or use with gateway builds that don't yet expose `gateway.identity.get`. The manual path uses the destructive `SetGatewayPubkey` (PUT `/api/gateways/{host}/pubkey`); auto-pin uses the additive-only TOFU path.

The bogus `cat /var/lib/openclaw/keys/public.pem` instruction in pre-FLEET-123 modal copy was wrong: OpenClaw stores its identity under `~/.openclaw` on the gateway host, with no user-readable PEM. Operators were never able to follow the original copy.

## Postures (FLEET-117 wizard collapses the toggles)

Three named postures map to canonical toggle combinations:

| Posture | auto-approve | OOB code | attest | Factors active | When to use |
|---|---|---|---|---|---|
| **Auto-pair** | ON | OFF | OFF | 1 — host token | Lab/dev, hosts with restricted token distribution |
| **Reviewed** *(default)* | OFF | OFF | ON-but-env-off | 2 — host token + operator | Production today. Attestation column staged for the OpenClaw RFC. |
| **Hardened** | OFF | ON | ON-with-env-on | 3 — host token + operator + gateway co-sign + OOB | When the gateway has the OpenClaw RFC and the operator has pasted the gateway pubkey |

Hardened is **gated** in the UI: the card renders with `🔒 Locked` until `gateway_pubkey_b64 != ''` AND a future `gateway_supports_oob_rpc` flag is true (hidden until OpenClaw ships).

Operators with non-canonical toggle combinations show as `Custom` in the wizard, with the advanced-toggle disclosure auto-expanded.

## Today's recommendation

For most production hosts, until the OpenClaw RFC ships:

| Toggle | Value | Reason |
|---|---|---|
| auto-approve | OFF | The only working second factor right now |
| OOB code | OFF | Gateway can't deliver codes yet → enabling forces SKIP OOB on every approval |
| attest | ON or OFF | Equivalent until the env flag is true. Default ON. |
| + PUBKEY | auto-pinned on first connect (TOFU, FLEET-123) | Manual paste remains as a fallback for rotation/recovery |

Equivalent posture: **Reviewed (default)**.

## When the OpenClaw RFC ships

1. Upgrade OpenClaw to a version that signs bridge registrations and emits OOB codes through the agent.
2. Paste the gateway's pubkey via the `+ PUBKEY` button (or wait for FLEET-117 to auto-capture it during pair).
3. Switch the gateway's posture from **Reviewed** to **Hardened** (one click in the FLEET-117 wizard, or flip both toggles manually).
4. Set `FLEETCOM_REGISTER_ATTESTATION_REQUIRED=true` in the FleetCom server env.
5. From now on, every new bridge registration requires the host token, a fresh gateway signature, AND a 6-digit code from the agent's user channel. N-of-3 compromise required.

## File references

| File | What |
|---|---|
| `backend/internal/db/db.go` | Schema for `openclaw_gateways` and `bridge_pairings` |
| `backend/internal/db/bridge_pairings.go` | OpenClawGateway / BridgePairing structs + per-toggle helpers |
| `backend/internal/api/bridges.go` | RegisterBridge (with attestation gate + OOB mint), ApproveBridge, ApproveBridgeSkipOOB, attestation verify, OOB hash + code generation |
| `backend/internal/api/info.go` | Public API catalog with all FLEET-111 endpoints |
| `backend/internal/openclaw/manager.go` | Per-gateway WS supervisor, auto-approver, PushConfirmationCode, RevokeBridgeOnGateway |
| `backend/internal/openclaw/identity.go` | Ed25519 keypair load/generate, FingerprintFromPubkeyPEM |
| `backend/internal/api/bridges_test.go` | Unit tests for the security primitives (12 tests covering verify happy/tampered/error-routing, hash salt-binding, env parsing) |
| `agent-bridge/cmd/agent-bridge/main.go` | Bridge container: registerLoop, fetchGatewaySignature (calls BRIDGE_GATEWAY_SIGN_URL when set) |
| `backend/static/index.html` | Frontend hooks (busy, confirmModal, fpHuman, oobRequiredForHost, attestation badges) and the host-drawer Integrations row |

## Threat model summary table

| Attacker has… | Without these layers | With Reviewed posture | With Hardened posture |
|---|---|---|---|
| Bosun host token (env leak) | Registers fake bridge, reads chats | Cannot — operator must approve and would see unfamiliar fingerprint | Cannot — gateway must endorse + OOB code must arrive |
| Bosun token + gateway compromise | Spoofs as any agent | Operator approval still required | Cannot — attacker doesn't control the agent's user channel |
| FleetCom admin session | Approves any bridge | Approves bridges (limited to gateways with auto-approve OFF — only matters for already-pending) | Same — but every approval is fingerprint-pinned + audit-logged + reversible |
| Compromised bridge container | Operator-scope on its local gateway (read-only intent; co-located blast radius — bridge + gateway share a host) | Same; compromise bounded by host-isolation, not bridge-vs-gateway | Same; bootstrap-token model (future) would scope to operator.read only |
| All three (host, gateway, FleetCom session) | Game over | Game over | Game over (acceptable design floor) |

The design floor is N=3 because going to N=4 requires either an external HSM (out of scope for self-hosted) or a multi-operator ceremony (operationally unrealistic).

## See also

- **FLEET-111** — security-primitives epic (this is what shipped)
- **FLEET-115** — operator-UX epic (wizard built on top of these primitives)
- **FLEET-125** — operator-session TLS = transport (Layer 0)
- **FLEET-126** — wire protocol fixes (frame.id, read-loop deadlock)
- **FLEET-127** — bridge-side mirror of the operator-session protocol fixes
- **FLEET-134** — bridge → gateway shared-secret short-circuit (the "bridge ↔ gateway connection" section above)
- **FLEET-149** — agent-name discipline (operator-asserted, no defaults)
- **FLEET-150** — UI hygiene (every agent display surface host-qualified)
- `docs/AGENT-BRIDGE-PAIRING.md` — the original (pre-FLEET-111) flow doc; superseded for the security model but still has accurate reference for the bridge-container side
- `docs/AGENT-OBSERVABILITY.md` — what flows through after pairing (per-turn telemetry)
