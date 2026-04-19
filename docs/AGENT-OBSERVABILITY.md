# Agent Observability (FLEET-36)

How FleetCom surfaces per-agent internal state — current turn, in-flight
tools, Telegram reply timestamps, errors — so the admin can distinguish
**silent-because-busy** from **silent-because-broken**.

This document is the canonical schema reference for any agent exporter.
Merlin (OpenClaw on hsb0) is the first implementation; Nimue and future
agents follow the same contract.

---

## Problem statement

Today FleetCom sees container-level health (up/down, OOM, health_status)
but no application-level signal for what an agent is doing. When a user
messages Merlin at 14:32 and no reply arrives by 14:44, the dashboard
looks "green" because:

- The container is up.
- The Telegram API is responsive.
- OpenRouter is healthy.
- The process hasn't crashed.

Yet Merlin might be busy on a 10-minute `claude-cli` session, or the
agent loop might be hung. Telling the two apart requires SSHing in and
grep-reading logs, which is ambiguous (`sendMessage ok` undercounts).

**Transparency goal**: every "quiet" state is explicable at a glance.
The admin never has to guess whether silence means work or failure.

---

## Principles

1. **Turns and tools are first-class** — not log lines. Structured data
   lets us render timelines, distributions, failure classes.
2. **Time pressure surfaced proactively** — if the Telegram typing
   indicator will expire in 30s and no reply is queued, warn *before*
   silence is observed.
3. **Causal chains** — user-msg → LLM → tool → reply, visualised as a
   span with timestamps.
4. **Known-unknowns are first-class** — "no turn completed in 1h"
   renders as a yellow-with-reason state, not green-because-absent.
5. **Generic over Merlin-specific** — schema fits any agent; Merlin is
   just the first exporter.
6. **Metadata by default** — raw LLM prompts/replies never stored;
   optional 120-char excerpts gated by a per-agent config flag.

---

## Transport — hybrid

Two channels, both authenticated with the host's Bosun bearer token.

### A. Snapshot (pull, on heartbeat)

The exporter serves a loopback HTTP endpoint:

```
GET /v1/agent-state
200 OK
Content-Type: application/json
```

Bosun fetches this each heartbeat (60s default) and attaches the JSON
to the heartbeat payload as `agent_states[]`. The server stores the
latest snapshot per agent in the `agents` table and broadcasts an
`agents` SSE event.

This gives dashboards an authoritative "NOW" view without any event
subscription.

### B. Events (push, real-time)

The exporter POSTs state-change events to the server:

```
POST /api/agent-events
Authorization: Bearer {host_token}
Content-Type: application/json

{ ... Event ... }
```

Used for precision: the dashboard sees `turn.started` within 1s, not
up to 60s later. Server writes to `agent_events`, updates
`agent_turns`/`agent_tools`, broadcasts an `agent-event` SSE event.

**Fire-and-forget semantics.** The exporter never blocks its agent
loop on a POST; a failed event is dropped after 3 retries. The
snapshot endpoint keeps the server's view eventually-consistent.

---

## Entities

### Agent

The unit of observability. Identified by `(host, name)`.

```json
{
  "host": "hsb0",
  "name": "merlin",
  "agent_type": "openclaw",
  "status": "tool-running",
  "status_since": "2026-04-19T14:32:05Z",
  "current_turn_id": "t_01HZ...",
  "typing": {
    "active": true,
    "chat_id": "-1001234567890",
    "expires_at": "2026-04-19T14:35:02Z"
  },
  "last_reply_per_chat": {
    "-1001234567890": "2026-04-19T14:25:01Z",
    "7654321": "2026-04-19T09:12:44Z"
  },
  "last_error": {
    "class": "rate-limit",
    "ts": "2026-04-19T13:08:22Z",
    "message_hash": "sha256:abcd..."
  },
  "rollups_24h": {
    "turns": 47,
    "errors": 2,
    "avg_turn_duration_ms": 3120,
    "p95_turn_duration_ms": 14800
  },
  "config_digest": "sha256:..."
}
```

### Status enum

| state | meaning | when trips |
|---|---|---|
| `idle` | agent waiting for input, no turn active | between turns |
| `receiving` | ingesting a new user message | Telegram msg received |
| `thinking` | LLM call in-flight | `llm.start` event |
| `tool-running` | subprocess/tool in-flight | `turn.tool-invoked` event |
| `replying` | emitting reply tokens | streaming reply begins |
| `error` | unrecoverable error on current turn | `turn.errored` event |
| `stuck` | turn started >N min ago, no tool, no event | auto-detect (see below) |

### Turn

One user ↔ agent exchange.

```json
{
  "id": "t_01HZ...",
  "agent": {"host": "hsb0", "name": "merlin"},
  "chat_id": "-1001234567890",
  "chat_name": "Markus ↔ Merlin",
  "started_at": "2026-04-19T14:32:05Z",
  "first_token_at": "2026-04-19T14:32:06Z",
  "replied_at": null,
  "status": "tool-running",
  "model": "anthropic/claude-sonnet-4.6",
  "tokens": {"prompt": 2401, "completion": null},
  "tools": ["clv_01HZ..."]
}
```

### Tool invocation

Nested under a turn.

```json
{
  "id": "clv_01HZ...",
  "turn_id": "t_01HZ...",
  "name": "claude-cli",
  "target": "hsb1",
  "started_at": "2026-04-19T14:32:08Z",
  "completed_at": null,
  "exit_code": null,
  "state": "running"
}
```

### Agent event (wire format)

All events share:

```json
{
  "agent": {"host": "hsb0", "name": "merlin"},
  "ts": "2026-04-19T14:32:05Z",
  "kind": "turn.started",
  "turn_id": "t_01HZ...",
  "payload": { ... kind-specific ... }
}
```

---

## Event vocabulary

| kind | triggered when | required payload |
|---|---|---|
| `turn.started` | user message received, agent begins processing | `chat_id`, `chat_name`, optional `excerpt` |
| `turn.tool-invoked` | subprocess/tool starts | `tool_id`, `name`, `target` |
| `turn.tool-completed` | subprocess/tool ends | `tool_id`, `exit_code`, `duration_ms` |
| `turn.replied` | reply successfully sent to user | `duration_ms`, `tokens_prompt`, `tokens_completion`, optional `excerpt` |
| `turn.errored` | turn fails unrecoverably | `class`, `message_hash`, optional `message` |
| `turn.abandoned` | turn orphaned by process death (post-crash catch-up) | `reason` |
| `typing.refreshed` | typing indicator re-sent | `chat_id`, `expires_at` |
| `config.changed` | model swap, prompt version bump, flag toggle | `config_digest_before`, `config_digest_after` |

**Error classes** (extensible, stable set): `rate-limit`, `api-auth`,
`api-5xx`, `timeout`, `tool-exit-nonzero`, `tool-unreachable`,
`internal`, `unknown`.

---

## Storage

New tables on the existing SQLite DB:

```sql
CREATE TABLE agents_obs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    agent_type TEXT NOT NULL DEFAULT '',
    snapshot_json TEXT NOT NULL DEFAULT '',
    snapshot_at TEXT NOT NULL DEFAULT '',
    UNIQUE(host_id, name)
);

CREATE TABLE agent_turns (
    id TEXT PRIMARY KEY,                    -- t_... (client-supplied ULID)
    agent_id INTEGER NOT NULL REFERENCES agents_obs(id) ON DELETE CASCADE,
    chat_id TEXT NOT NULL DEFAULT '',
    chat_name TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    first_token_at TEXT,
    replied_at TEXT,
    status TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    tokens_prompt INTEGER,
    tokens_completion INTEGER,
    duration_ms INTEGER,
    error_class TEXT,
    excerpt TEXT NOT NULL DEFAULT ''        -- optional, gated by exporter flag
);
CREATE INDEX idx_agent_turns_agent_started ON agent_turns(agent_id, started_at DESC);

CREATE TABLE agent_tools (
    id TEXT PRIMARY KEY,                    -- clv_... (client-supplied ULID)
    turn_id TEXT NOT NULL REFERENCES agent_turns(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    completed_at TEXT,
    exit_code INTEGER,
    duration_ms INTEGER
);
CREATE INDEX idx_agent_tools_turn ON agent_tools(turn_id);

CREATE TABLE agent_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id INTEGER NOT NULL REFERENCES agents_obs(id) ON DELETE CASCADE,
    ts TEXT NOT NULL,
    kind TEXT NOT NULL,
    turn_id TEXT,
    payload_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_agent_events_agent_ts ON agent_events(agent_id, ts DESC);
CREATE INDEX idx_agent_events_ts ON agent_events(ts);
```

### Retention

Matches `status_samples`: **400 days rolling** on `agent_events`,
`agent_turns`, `agent_tools`. Snapshot row in `agents_obs` is always
the latest — no retention.

Pruning runs alongside the existing `PurgeOldSamples` ticker.

---

## STUCK detection

Client-side (dashboard) + server-side derivation of the `stuck` state.

**Rule**: a turn is STUCK when ALL of the following are true:

1. `status_since` is in the past by more than `stuck_threshold_seconds`
   (default **120s**).
2. No `turn.tool-invoked`, `turn.tool-completed`, or `typing.refreshed`
   event has been received for this turn in the last
   `stuck_silence_seconds` (default **120s**).
3. `status` ∈ {`thinking`, `tool-running`, `replying`} (STUCK does not
   apply to `idle`, `receiving`, `error`).

Configurable per-agent via the snapshot's optional
`config.stuck_thresholds` block.

**Visual effect**:

- Chip turns red, reads `STUCK <dur> · <reason>`.
- Host card border pulses red.
- Sticky toast at bottom.
- Optional desktop notification (Notification API, permission gated).

**De-trip**: clears when any of the above conditions becomes false.

---

## Privacy

- **Default**: snapshot + events carry metadata only. Chat names are
  stored verbatim (they're the human-readable chat title); user IDs
  are stored as opaque strings.
- **Excerpts**: disabled by default. Per-agent `emitExcerpts: true`
  config flag opts in. Max 120 chars, truncated by the exporter.
- **Error messages**: hashed by default. Raw messages stored only when
  `emitErrorText: true`.
- **Raw LLM I/O**: never stored by FleetCom. Debugging LLM I/O is
  out-of-scope; use OpenClaw's own logs.

---

## Authentication

Agent events use the **same bearer token** the host's Bosun uses. The
exporter runs co-located with Bosun (same host) and shares the env
variable `FLEETCOM_TOKEN`. The server identifies which agent from the
`agent: {host, name}` field, but validates that `host` matches the
token's hostname — preventing an exporter on host A from writing events
for host B.

---

## Versioning

Schema version is carried in the snapshot root:

```json
{ "schema_version": 1, "agents": [ ... ] }
```

Breaking changes bump the major. Server accepts schema_versions it
knows how to parse; unknown versions return 400.

---

## Minimal exporter checklist

To add a new agent exporter:

1. Serve `GET /v1/agent-state` returning the schema above, bound to
   loopback or internal container network.
2. On each state change, `POST /api/agent-events` with the event body
   (see event vocabulary). Fire-and-forget.
3. Set `OPENCLAW_STATE_URL` (or `AGENT_STATE_URL`) env var on the host's
   Bosun service, pointing at your `/v1/agent-state`.
4. Share `FLEETCOM_TOKEN` with the exporter so it can POST events.
5. Emit `typing.refreshed` events for any chat where a proactive typing
   indicator is used.
6. Implement `turn.abandoned` backfill on exporter restart so crash
   recovery doesn't leave zombie "tool-running" turns.
