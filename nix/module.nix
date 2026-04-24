{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.fleetcom-bosun;

  agentsJson = builtins.toJSON (
    map (a: {
      name = a.name;
      agent_type = a.type;
      status = "online";
    }) cfg.agents
  );

  agentScript = pkgs.writeShellScript "fleetcom-bosun" ''
    set -euo pipefail

    TOKEN=$(cat "$FLEETCOM_TOKEN_FILE")

    INTERVAL=60

    trap 'exit 0' SIGTERM SIGINT

    while true; do
        HOSTNAME_VAL=$(${pkgs.hostname}/bin/hostname)
        KERNEL=$(${pkgs.coreutils}/bin/uname -r)

        # OS name
        if [ -f /etc/os-release ]; then
            OS=$(. /etc/os-release && echo "''${PRETTY_NAME:-$NAME}")
        else
            OS=$(${pkgs.coreutils}/bin/uname -s)
        fi

        # Uptime
        UPTIME=$(${pkgs.gawk}/bin/awk '{printf "%d", $1}' /proc/uptime)

        # Auto-discover Docker containers
        CONTAINERS="[]"
        if command -v docker &>/dev/null; then
            CONTAINERS=$(docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.State}}' 2>/dev/null | \
                ${pkgs.gawk}/bin/awk -F'\t' 'BEGIN{printf "["} NR>1{printf ","} {printf "{\"name\":\"%s\",\"image\":\"%s\",\"state\":\"%s\"}", $1, $2, $3} END{printf "]"}' 2>/dev/null || echo "[]")
            [ "$CONTAINERS" = "[]" ] || [ -z "$CONTAINERS" ] && CONTAINERS="[]"
        fi

        AGENTS='${agentsJson}'

        RESPONSE=$(${pkgs.curl}/bin/curl -sf --max-time 10 \
            -X POST "${cfg.url}/api/heartbeat" \
            -H "Authorization: Bearer $TOKEN" \
            -H "Content-Type: application/json" \
            -d "{\"hostname\":\"$HOSTNAME_VAL\",\"os\":\"$OS\",\"kernel\":\"$KERNEL\",\"uptime_seconds\":$UPTIME,\"containers\":$CONTAINERS,\"agents\":$AGENTS}" \
            2>/dev/null) || true

        if [ -n "$RESPONSE" ]; then
            NEW_INTERVAL=$(echo "$RESPONSE" | ${pkgs.gnugrep}/bin/grep -o '"interval":[0-9]*' | ${pkgs.gnugrep}/bin/grep -o '[0-9]*' || true)
            if [ -n "$NEW_INTERVAL" ] && [ "$NEW_INTERVAL" -ge 10 ] 2>/dev/null; then
                INTERVAL=$NEW_INTERVAL
            fi
        fi

        sleep "$INTERVAL"
    done
  '';

in
{
  options.services.fleetcom-bosun = {
    enable = lib.mkEnableOption "FleetCom heartbeat bosun";

    url = lib.mkOption {
      type = lib.types.str;
      default = "https://fleet.barta.cm";
      description = "FleetCom server URL.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to file containing the bearer token (e.g. agenix secret path).";
      example = lib.literalExpression "config.age.secrets.fleetcom-token.path";
    };

    agents = lib.mkOption {
      type = lib.types.listOf (
        lib.types.submodule {
          options = {
            name = lib.mkOption {
              type = lib.types.str;
              description = "Agent display name.";
            };
            type = lib.mkOption {
              type = lib.types.str;
              default = "assistant";
              description = "Agent type (e.g. cto, assistant, social-media).";
            };
          };
        }
      );
      default = [ ];
      description = "AI agents running on this host.";
      example = lib.literalExpression ''
        [
          { name = "Merlin"; type = "cto"; }
          { name = "Nimue"; type = "assistant"; }
        ]
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.tokenFile != "";
        message = "services.fleetcom-bosun.tokenFile must be set.";
      }
    ];

    systemd.services.fleetcom-bosun = {
      description = "FleetCom heartbeat bosun";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      environment = {
        FLEETCOM_TOKEN_FILE = toString cfg.tokenFile;
        # FLEET-78: soft heap limit so the Go runtime returns memory to
        # the OS aggressively. Paired with MemoryMax in serviceConfig.
        GOMEMLIMIT = "200MiB";
      };

      path = [ "/run/wrappers" "/run/current-system/sw" ];

      serviceConfig = {
        Type = "simple";
        ExecStart = agentScript;
        Restart = "on-failure";
        RestartSec = "10s";
        User = "root"; # needs docker access
        # FLEET-78: cgroup hard cap so a runaway bosun gets OOM-killed
        # by systemd instead of thrashing the host into swap.
        MemoryMax = "256M";
        MemorySwapMax = "0";
      };
    };
  };
}
