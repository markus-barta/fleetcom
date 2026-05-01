# Operations Runbook

Operational notes for keeping FleetCom and its agents healthy.

## Memory Baselines

The following are healthy steady-state RSS values per process. Sustained
deviation above the alert threshold means something is leaking and
warrants investigation before the host runs out of RAM.

| Process               | Container          | Baseline RSS | Alert at | Hard cap (cgroup) |
| --------------------- | ------------------ | ------------ | -------- | ----------------- |
| `fleetcom-server`     | `fleetcom`         | 15–25 MiB    | 100 MiB  | unset (web stack) |
| `fleetcom-bosun`      | `fleetcom-bosun`   | 20–60 MiB    | 150 MiB  | 256 MiB           |
| `fleetcom-agent-bridge` | `fleetcom-agent-bridge` | 10–25 MiB | 80 MiB   | unset             |

**Why this matters.** The 2026-04-25 incident on `cs1.barta.cm` (FLEET-78)
was caused by the bosun agent reaching **2.76 GiB** RSS over six days. The
host was not OOM-killed because it had 5.9 GiB of swap; instead the
kernel thrashed and SSH was unable to fork new sessions for several
minutes. Setting a hard memory cap on the agent container ensures the
kernel kills bosun before it can take down sshd.

## Detecting a Leak

### Quick check on a single host

    docker stats --no-stream fleetcom-bosun

`MEM USAGE` should stay well under 100 MiB on a host with a normal
container count (~30 containers). If you see 200+ MiB, capture state
before restarting:

    docker logs --tail 200 fleetcom-bosun > /tmp/bosun.log
    docker exec fleetcom-bosun /usr/local/bin/fleetcom-bosun --version
    docker stats --no-stream > /tmp/all-stats.txt
    # Restart to recover
    docker restart fleetcom-bosun
    # Verify
    docker stats --no-stream fleetcom-bosun

### Fleet-wide check

The dashboard's Hosts panel surfaces each host's `agent_rss_bytes` from
the `hw_live` heartbeat block (FLEET-37). Sort by `Agent RSS` desc to
find outliers.

## Memory Limits

The agent runs with a hard cap configured in `agent/docker-compose.yml`:
`mem_limit: 256m` + `memswap_limit: 256m` (no swap allowed).

The Go runtime is also given a soft heap target via `GOMEMLIMIT=200MiB`
(~80% of the cgroup cap). Below that, the GC runs aggressively and
returns pages to the OS instead of waiting for kernel memory pressure.

## Runbook: SSH Unresponsive but Host Pings

Symptoms: ICMP works, SSH connects but never gets to the password
prompt, or hangs after auth. Load average is high.

This is the FLEET-78 footprint (out-of-memory thrashing rather than a
crash). On a host you still have console access to (Hetzner Cloud
console, IPMI, etc.):

1. `free -h` — if `Swap` is 100% used and `Mem.available` is < 100 MiB,
   you're memory-starved.
2. `ps auxww --sort=-rss | head -20` — find the top RSS consumer.
   If it's `fleetcom-bosun`, `docker restart fleetcom-bosun` reclaims
   the memory immediately.
3. `docker stats --no-stream` confirms the post-restart baseline.

If `fleetcom-bosun` is **not** the top offender, this runbook does not
apply — investigate the actual top process.

## Releasing a Bosun Fix

1. Patch-bump `AGENT_VERSION` (and `VERSION` if the server also changed)
   in `.github/workflows/ci.yml`. Bump the **minor** segment for feature
   releases (e.g. `0.4.x` → `0.5.0` when shipping FLEET-83).
2. Push to `main`. CI builds + publishes
   `ghcr.io/markus-barta/fleetcom-bosun:latest` (plus a `:vX.Y.Z` tag
   and an `agent-vX.Y.Z` GitHub Release for the cross-compiled native
   binaries).
3. The dashboard's per-host **"Update agent"** button is the primary
   path for triggering an upgrade. It dispatches by each host's
   `deployment_shape` (FLEET-84):

   | Shape                | What "Update agent" does                                                                |
   | -------------------- | --------------------------------------------------------------------------------------- |
   | `docker+watchtower`  | POSTs to the host's watchtower HTTP API, which pulls + recreates fleetcom-bosun.        |
   | `docker-bare`        | Enqueues an `agent.update` command. Bosun spawns a one-shot helper container that      |
   |                      | does `docker stop` + `docker rm` + `docker run` with the inspected spec preserved      |
   |                      | (env, mounts, labels including compose, network, ports). Compose-aware for free        |
   |                      | because the labels survive — next `docker compose up` from the host still recognises   |
   |                      | the container as project-managed. (FLEET-86)                                            |
   | `systemd-native`     | Bosun downloads the matching ARMv6/arm64/amd64 binary from the GitHub Release,         |
   |                      | verifies SHA-256, atomic-mvs onto `/usr/local/bin/fleetcom-{bosun,agent}`, then        |
   |                      | `systemctl restart --no-block <unit>`. (FLEET-87)                                       |
   | `unknown`            | Button is disabled; tooltip explains "manual update required". The dashboard will not  |
   |                      | silently fire a no-op (the failure mode that bit hsb0 pre-FLEET-83).                    |

   Server reconciles the `restarting` command to `done` automatically
   when the new bosun's first heartbeat reports a different
   `agent_version` than was captured at enqueue time. If the new
   version doesn't return within 5 min, `ExpireStuckCommands` flips the
   command to `failed` with a timeout error so the operator sees it.

4. **Manual fallback** (Path B) when the dashboard route can't be used —
   bootstrapping a host onto a v0.5.0+ binary that has the FLEET-87
   handler in the first place, or recovering from a botched in-process
   update:

   ```
   # Docker hosts
   ssh <host>
   cd <compose-dir>            # nixcfg/hosts/<host>/docker is the standard;
                               # /opt/fleetcom-agent/ on legacy unmigrated hosts
                               # (cf. NIX-76, DSC26-60).
   sudo docker compose pull fleetcom-agent && sudo docker compose up -d fleetcom-agent

   # Native systemd hosts (Pi etc.)
   ssh <host>
   sudo AGENT_VERSION=<X.Y.Z> bash <(curl -fsSL \
     https://raw.githubusercontent.com/markus-barta/fleetcom/main/agent/install-native.sh)
   ```

   The dashboard's per-host Commands tab shows the audit trail either
   way — failures from in-process updates are recorded there with the
   bosun-side error.

5. **Deployment-shape coverage.** As of 2026-05-01 the fleet runs:
   `csb0`, `csb1`, `gpc0`, `hsb1`, `hsb8` on `docker+watchtower`;
   `hsb0`, `dsc0`, `msbp` on `docker-bare`; `hsb2` on `systemd-native`.
   Hosts where bosun reports `unknown` (no Docker socket visible from
   inside the container, no systemd unit on the host) need their
   deployment hardened before they can self-update — the badge on the
   host card will flag them.
