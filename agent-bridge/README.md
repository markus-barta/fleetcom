# fleetcom-agent-bridge

Reference **agent exporter** for FleetCom. Runs next to an agent
runtime (OpenClaw on hsb0 is the first target), converts runtime
activity into the generic agent-observability events defined in
[`../docs/AGENT-OBSERVABILITY.md`](../docs/AGENT-OBSERVABILITY.md),
and exposes the two surfaces FleetCom needs:

- **`GET /v1/agent-state`** — pulled by Bosun each heartbeat (60s).
- **`POST /api/agent-events`** (to FleetCom) — real-time push of
  turn/tool/typing/error lifecycle events.

The MVP event source is **docker log tailing** with configurable regex
patterns ([`patterns.go`](cmd/agent-bridge/patterns.go)). Point it at
any container whose logs emit structured lines and you get agent
observability without modifying the agent runtime itself.

## Why a bridge instead of patching OpenClaw?

OpenClaw's internal agent loop isn't yet emitting the FleetCom event
schema. Rather than fork OpenClaw, we run a small sidecar that
observes what it already logs. When OpenClaw (or any runtime) gains
native emitters, the bridge becomes unnecessary — swap it out for
direct push.

## Configuration

| env var | default | purpose |
|---|---|---|
| `FLEETCOM_URL` | `https://fleet.barta.cm` | Base URL to POST events to |
| `FLEETCOM_TOKEN` | *(required for push)* | Shared with Bosun on this host |
| `FLEETCOM_HOSTNAME` | os hostname | Identity reported in events |
| `BRIDGE_LOG_CONTAINER` | `openclaw-gateway` | Container whose logs to tail |
| `BRIDGE_AGENT_NAMES` | `merlin,nimue` | Agents to track |
| `BRIDGE_AGENT_TYPE` | `openclaw` | Reported as `agent_type` |
| `BRIDGE_BIND_ADDR` | `:9180` | Where `/v1/agent-state` binds |

## Wire it to Bosun

Add `OPENCLAW_STATE_URL=http://agent-bridge:9180/v1/agent-state` to
the `fleetcom-bosun` service's environment (see
[`docker-compose.sample.yml`](docker-compose.sample.yml)).

## Adjusting log patterns

The bridge recognises these line shapes out of the box:

```
turn.started agent=merlin turn=t_01H... chat=-1001 name="Markus ↔ Merlin"
turn.tool_invoked agent=merlin turn=t_01H... tool=clv_01H... name=claude-cli target=hsb1
turn.tool_completed agent=merlin turn=t_01H... tool=clv_01H... exit=0 dur=12340ms
turn.replied agent=merlin turn=t_01H... dur=12840ms tok=2401/812
turn.errored agent=merlin turn=t_01H... class=rate-limit
typing.refreshed agent=merlin chat=-1001 exp=2026-04-19T14:35:02Z
```

If OpenClaw (or your agent) logs a different format, edit the
regexes in [`patterns.go`](cmd/agent-bridge/patterns.go) — no schema
change needed on the FleetCom side.

## Non-features (by design)

- **No event buffering to disk.** Fire-and-forget with 3 in-memory
  retries. If FleetCom is down for >1s per event, those events drop.
  The snapshot scraped by Bosun keeps the overall state eventually
  consistent.
- **No LLM I/O capture.** Payloads are metadata-only unless the
  source line itself carries an excerpt; raw prompts/replies never
  enter FleetCom.
- **No multi-host.** One bridge per host; each agent must run on a
  host that has both the agent runtime and this bridge.
