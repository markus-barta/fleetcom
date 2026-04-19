# Agent-Bridge Pairing Architecture (FLEET-50/51/52)

How FleetCom pairs with OpenClaw gateways once (per host) and then
auto-approves every agent-bridge that boots on those hosts, resulting
in a zero-touch deployment experience: `docker compose up -d` and the
dashboard starts reporting within seconds.

**Goal:** no `openclaw pair approve` commands, no SSH into gateway
hosts, no Control UI clicks. Admins add a host to FleetCom (existing
flow, gets a bosun token) and run the standard nixcfg deploy. The rest
is automatic.

## The three tickets

| ticket | layer | what it delivers |
|---|---|---|
| **FLEET-50** | agent-bridge | Ed25519-signed WS client that connects to OpenClaw, subscribes to `sessions.messages`, translates to the agent-observability schema. Replaces the failed log-tail approach. |
| **FLEET-51** | fleetcom-server + UI | Per-gateway keypair management, gateway registry, bridge registration endpoint, auto-approver, dashboard UI for manual approval / revocation fallback. |
| **FLEET-52** | nixcfg | Pre-seeds FleetCom's pubkey + operator token into OpenClaw's `paired.json` at deploy time. Makes the initial FleetCom↔gateway pairing zero-touch too. |

Shipped together, they deliver true zero-touch. Any one can land alone
and the others degrade gracefully.

## End-to-end flow (happy path, all three live)

```
 ┌─── FleetCom server ─────────────────────────────────────┐
 │  keypair_hsb0  (agenix: fleetcom-openclaw-hsb0-key.age) │
 │  token_hsb0    (agenix: fleetcom-openclaw-hsb0-tok.age) │
 │                                                          │
 │  openclaw_gateways:                                      │
 │    - host: hsb0, url: wss://hsb0:18789, paired_at: ...   │
 │                                                          │
 │  bridge_pairings:                                        │
 │    - host: hsb0, agent: merlin, pubkey_fp: ..., status:  │
 │      auto-approved                                        │
 └──┬───────────────────────────────────────────▲──────────┘
    │  (FleetCom connects with signed handshake) │
    │  (scopes: operator.read, operator.pairing) │
    ▼                                            │
 ┌───── OpenClaw gateway on hsb0 ─────────────┐  │
 │  paired.json contains FleetCom's pubkey    │  │
 │  + operator token (pre-seeded by nixcfg)   │  │ device.pair.approve
 │                                             │  │ auto-called
 │  pending queue receives bridge's request ─┼──┘
 │  (fingerprint matches a registration)      │
 └───────────────▲─────────────────────────────┘
                 │ Ed25519-signed
                 │ handshake, scopes granted
                 │
 ┌───── agent-bridge on hsb0 ─────────┐
 │  keypair in /var/lib/agent-bridge/ │
 │  POSTs fingerprint to FleetCom     │   bosun token authenticates
 │  (bearer: FLEETCOM_TOKEN) ─────────┼──▶  /api/bridges/register
 │                                    │
 │  Then connects to gateway with     │
 │  signed handshake                  │
 └────────────────────────────────────┘
```

Steps on a cold new host (after nixcfg apply):

1. **Bosun** boots, heartbeats to FleetCom with the host's bearer token. FleetCom already has this host registered (existing flow).
2. **agent-bridge** boots.
   - If keypair missing → generate Ed25519 pair, persist to `/var/lib/fleetcom-agent-bridge/keys/{private.pem, public.pem}`.
   - POST fingerprint + pubkey to FleetCom: `POST /api/bridges/register` with bosun bearer. FleetCom records `bridge_pairings[host=hsb0, agent=merlin, fp=...]`.
3. **agent-bridge** opens WS to `wss://127.0.0.1:18789`, sends the signed `connect` frame. If gateway already knows the key (re-start), handshake succeeds immediately. Otherwise gateway puts the request in `device.pair.pending`.
4. **FleetCom's gateway client** (already connected with scopes from step 0) subscribes to pairing events. A pending request with a fingerprint matching any row in `bridge_pairings` for that host → auto-call `device.pair.approve(requestId)`.
5. **agent-bridge** reconnects (backoff loop), signed handshake succeeds, subscribes to `sessions.messages`, translates events to FleetCom's schema, POSTs them to `POST /api/agent-events`.

First heartbeat → first agent-event land in the dashboard typically within **3–10 seconds** of container start.

## Auth & trust model

| actor | secret held | stored at |
|---|---|---|
| host bosun | bearer token (per-host, created when host is added to FleetCom) | agenix: `fleetcom-token-<host>.age` |
| agent-bridge | Ed25519 keypair (per-bridge, generated on first boot) | volume: `/var/lib/fleetcom-agent-bridge/keys/` |
| FleetCom server | Ed25519 keypair + operator token (per-gateway) | agenix: `fleetcom-openclaw-<host>-key.age` + `fleetcom-openclaw-<host>-tok.age`, mounted into fleetcom container |

**Trust chain for auto-approval:** a bridge is auto-approved on gateway X if, and only if, a POST to `/api/bridges/register` was received for that host with a valid bosun bearer token AND the fingerprints match. "The bosun token is trusted for this host" is the anchor.

**Dashboard override:** each gateway row has an `auto_approve_bridges` toggle (default on). When off, requests surface in the UI with an Approve button; nothing auto-approves.

**Revocation:** `DELETE /api/bridges/{host}/{agent}` calls `device.token.revoke` on the gateway, drops the bridge_pairings row, bridge reconnects into pending (and gets auto-approved again unless you've also deleted the registration — idempotent by design).

## Storage

### FleetCom DB tables (new)

```sql
CREATE TABLE openclaw_gateways (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host TEXT NOT NULL UNIQUE,         -- matches hosts.hostname
    url TEXT NOT NULL,                 -- wss://host:18789
    fc_pubkey_b64 TEXT NOT NULL DEFAULT '',    -- FleetCom's public key for this gateway
    fc_device_token_hash TEXT NOT NULL DEFAULT '', -- sha256 of operator token (for audit)
    paired_at TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unpaired',   -- unpaired | paired | revoked
    auto_approve_bridges INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE bridge_pairings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host TEXT NOT NULL,
    agent TEXT NOT NULL,
    pubkey_fp TEXT NOT NULL,
    pubkey_pem TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',    -- pending | approved | revoked
    approved_at TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',        -- gateway-side requestId when known
    UNIQUE(host, agent)
);
```

### agenix layout (per-gateway)

```
secrets/fleetcom-openclaw-hsb0-key.age      # Ed25519 private key PEM
secrets/fleetcom-openclaw-hsb0-pubkey.age   # Ed25519 public key PEM (for nixcfg pre-seed)
secrets/fleetcom-openclaw-hsb0-tok.age      # operator token (for paired.json pre-seed)
```

Files mounted read-only into fleetcom container at `/run/agenix/fleetcom-openclaw-<host>-*`. FleetCom loads per-gateway keys lazily when first connecting.

### OpenClaw trust store pre-seed (FLEET-52)

`/home/node/.openclaw/devices/paired.json` gets a FleetCom entry at OpenClaw container boot. nixcfg entrypoint merges:

```json
{
  "<deviceId>": {
    "deviceId": "<sha256 hex of pubkey bytes>",
    "publicKey": "<base64url pubkey>",
    "platform": "linux",
    "clientId": "gateway-client",
    "clientMode": "backend",
    "role": "operator",
    "roles": ["operator"],
    "scopes": ["operator.read", "operator.pairing"],
    "approvedAt": <boot-time ms>,
    "tokens": {
      "operator": {
        "token": "<agenix-sourced operator token>",
        "role": "operator",
        "scopes": ["operator.read", "operator.pairing"],
        "createdAtMs": <boot-time ms>
      }
    }
  }
}
```

Merge semantics: preserve any other entries already present (e.g. the Control UI's own pairing). Idempotent — re-applying the merge leaves the file unchanged.

## Endpoints

```
POST /api/bridges/register
  bearer: host bosun token
  body: { agent: "merlin", pubkey_pem, pubkey_fingerprint }
  result: { ok, status: "registered", auto_approve: bool }

GET /api/gateways                (admin)
  result: [{ host, status, paired_at, auto_approve_bridges }]

POST /api/gateways/{host}/pair/approve/{requestId}   (admin)
  result: { ok, device_id }

POST /api/gateways/{host}/auto-approve/{on|off}   (admin)

GET /api/bridges                 (admin)
  result: [{ host, agent, status, approved_at, last_seen }]

DELETE /api/bridges/{host}/{agent}   (admin — revoke)
```

SSE: new events `gateways` (list changes) and `bridges` (pairing state changes).

## Dashboard

Two new tabs under Settings:

- **Gateways** — list every OpenClaw gateway seen on any host. Per-row: host, pair status, copy-paste bootstrap command if unpaired (FLEET-51 fallback for when FLEET-52 isn't deployed yet), auto-approve toggle, revoke button.
- **Bridges** — list every agent-bridge. Per-row: host/agent, fingerprint, last-seen, status, revoke button. Pending requests show an Approve button for the manual fallback.

## Failure modes & mitigations

| failure | what happens | mitigation |
|---|---|---|
| FleetCom offline during bridge register | bridge retries every 30s until registration accepted | bosun's existing reconnect logic; no data loss |
| Gateway offline | FleetCom can't push events out but keeps trying; bridge can't connect | normal reconnect with backoff |
| Pubkey mismatch on reconnect | gateway rejects; bridge regenerates keypair + re-registers | rare, only on volume wipe |
| FleetCom keypair compromised | impersonation on all gateways | rotate via agenix + nixcfg re-apply (wipes old entry from `paired.json`, writes new) |
| Bridge bosun token rotated | bridge's registration invalidated | bridge re-registers with the new bearer automatically |
| OpenClaw upgrade changes `paired.json` schema | pre-seed may misalign | nixcfg entrypoint probes format first; falls back to manual-command path on schema drift (with a dashboard warning) |

## Rollout order

1. **FLEET-51 first** — server + UI. Without this, FLEET-50 can't self-approve.
2. **FLEET-50 second** — bridge rewrite. Once server side can approve, bridge becomes zero-touch.
3. **FLEET-52 last** — nixcfg pre-seed. Turns "1 command per new gateway" into "0 commands".

Each stage is usable on its own:

- After FLEET-51: FleetCom has Gateways + Bridges UI, but pairing still requires 1 manual command per gateway + per bridge (until FLEET-50 lands the auto-approval path).
- After FLEET-50: bridges auto-approve via FleetCom; gateway-pairing still needs the 1 command per gateway.
- After FLEET-52: zero manual commands for the whole lifecycle.

## Open questions for implementation

- Does the server's Ed25519 signing use Node-compat format (OpenClaw expects a specific envelope — `buildDeviceAuthPayloadV3`). Verify payload shape in Go before wiring.
- Token refresh: `device.token.rotate` exists; rotation policy TBD (monthly? on compromise only?).
- What happens to already-paired bridges when FleetCom rotates its own keypair on a gateway? Probably need a graceful re-register dance or a manual re-pair.
