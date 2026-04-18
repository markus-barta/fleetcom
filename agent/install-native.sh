#!/usr/bin/env bash
# Install or update fleetcom-bosun on a host that can't run Docker
# (e.g. Raspberry Pi running Raspbian). Grabs the matching binary from
# the latest GitHub Release and swaps it in-place under systemd.
#
# Usage:   sudo AGENT_VERSION=0.1.3 bash install-native.sh
#          (AGENT_VERSION defaults to "latest" which follows the rolling tag.)
#
# Requires: curl, tar (optional), systemd, /etc/fleetcom/env with the
# FLEETCOM_URL + FLEETCOM_TOKEN + FLEETCOM_HOSTNAME vars.
set -euo pipefail

VERSION="${AGENT_VERSION:-latest}"
TAG="${VERSION:+agent-v$VERSION}"
[ "$VERSION" = "latest" ] && TAG="latest"

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)          ASSET="fleetcom-bosun-linux-amd64" ;;
    aarch64|arm64)   ASSET="fleetcom-bosun-linux-arm64" ;;
    armv6l|armv7l)   ASSET="fleetcom-bosun-linux-armv6" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

BASE="https://github.com/markus-barta/fleetcom/releases"
if [ "$TAG" = "latest" ]; then
    URL="$BASE/latest/download/$ASSET"
else
    URL="$BASE/download/$TAG/$ASSET"
fi

echo "fetching $URL"
TMP="$(mktemp)"
curl -fsSL -o "$TMP" "$URL"
curl -fsSL -o "$TMP.sha256" "$URL.sha256" || true

if [ -s "$TMP.sha256" ]; then
    EXPECTED="$(awk '{print $1}' "$TMP.sha256")"
    ACTUAL="$(sha256sum "$TMP" | awk '{print $1}')"
    if [ "$EXPECTED" != "$ACTUAL" ]; then
        echo "checksum mismatch; aborting" >&2; exit 2
    fi
fi

install -m 755 "$TMP" /usr/local/bin/fleetcom-agent
rm -f "$TMP" "$TMP.sha256"

if systemctl list-unit-files fleetcom-agent.service >/dev/null 2>&1; then
    systemctl restart fleetcom-agent
    systemctl --no-pager --lines=5 status fleetcom-agent || true
else
    echo "note: no fleetcom-agent.service found — binary installed but not running."
    echo "      Create /etc/fleetcom/env and a systemd unit first."
fi
