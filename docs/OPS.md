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

The agent runs with a hard cap that covers both Docker compose
deployments and the NixOS systemd unit:

- **Docker compose** (`agent/docker-compose.yml`):
  `mem_limit: 256m` + `memswap_limit: 256m` (no swap allowed)
- **NixOS module** (`nix/module.nix`):
  `MemoryMax = "256M"` + `MemorySwapMax = "0"`

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

1. Patch-bump `BOSUN_VERSION` in `.github/workflows/ci.yml`.
2. Push to `main`. CI builds + publishes
   `ghcr.io/markus-barta/fleetcom-bosun:latest` (and a digest tag).
3. On each host, the bundled or external Watchtower picks up the new
   image on its next poll, or the operator can trigger an immediate
   pull from the FleetCom dashboard ("Update now" on the host drawer).
