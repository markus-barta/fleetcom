#!/usr/bin/env bash
# FleetCom heartbeat agent
# Runs via cron every 60s, POSTs system state to FleetCom server.
#
# Configuration (environment variables):
#   FLEETCOM_URL    — Server URL (e.g. https://fleet.barta.cm)
#   FLEETCOM_TOKEN  — Per-host bearer token
#   FLEETCOM_AGENTS — Optional JSON array of agents on this host
#                     e.g. '[{"name":"Ocean","agent_type":"cto","status":"online"}]'
#
# Cron entry:
#   * * * * * FLEETCOM_URL=https://fleet.barta.cm FLEETCOM_TOKEN=xxx /path/to/fleetcom-agent.sh

set -euo pipefail

: "${FLEETCOM_URL:?FLEETCOM_URL not set}"
: "${FLEETCOM_TOKEN:?FLEETCOM_TOKEN not set}"

HOSTNAME=$(hostname)
OS=$(uname -s)
KERNEL=$(uname -r)

# Detect OS name
if [ -f /etc/os-release ]; then
    OS=$(. /etc/os-release && echo "${PRETTY_NAME:-$NAME}")
elif [ "$(uname)" = "Darwin" ]; then
    OS="macOS $(sw_vers -productVersion 2>/dev/null || echo '')"
fi

# Uptime in seconds
if [ -f /proc/uptime ]; then
    UPTIME=$(awk '{printf "%d", $1}' /proc/uptime)
elif [ "$(uname)" = "Darwin" ]; then
    BOOT=$(sysctl -n kern.boottime | awk '{print $4}' | tr -d ,)
    NOW=$(date +%s)
    UPTIME=$((NOW - BOOT))
else
    UPTIME=0
fi

# Docker containers
CONTAINERS="[]"
if command -v docker &>/dev/null; then
    CONTAINERS=$(docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.State}}' 2>/dev/null | \
        awk -F'\t' 'BEGIN{printf "["} NR>1{printf ","} {printf "{\"name\":\"%s\",\"image\":\"%s\",\"state\":\"%s\"}", $1, $2, $3} END{printf "]"}' 2>/dev/null || echo "[]")
    # Handle empty output
    [ "$CONTAINERS" = "[]" ] || [ -z "$CONTAINERS" ] && CONTAINERS="[]"
fi

# Agents (from env var or empty)
AGENTS="${FLEETCOM_AGENTS:-[]}"

# Build JSON payload
PAYLOAD=$(cat <<EOF
{
    "hostname": "${HOSTNAME}",
    "os": "${OS}",
    "kernel": "${KERNEL}",
    "uptime_seconds": ${UPTIME},
    "containers": ${CONTAINERS},
    "agents": ${AGENTS}
}
EOF
)

# Send heartbeat
curl -sf --max-time 10 \
    -X POST "${FLEETCOM_URL}/api/heartbeat" \
    -H "Authorization: Bearer ${FLEETCOM_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "${PAYLOAD}" \
    >/dev/null 2>&1 || true
