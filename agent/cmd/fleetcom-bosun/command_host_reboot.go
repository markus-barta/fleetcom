package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// handleHostReboot triggers an orderly host reboot via the same path
// `systemctl reboot` uses: a D-Bus call to org.freedesktop.login1.Manager
// .Reboot. This avoids needing --privileged or --pid=host on the bosun
// container — only the system bus socket bind-mount and stock polkit
// defaults (which permit uid 0 to reboot without prompting).
//
// FLEET-369.1. The handler:
//
//  1. Pre-flight: confirm the docker-compose'd /var/run/dbus/system_bus_
//     _socket bind exists and connect to the system bus.
//  2. Call org.freedesktop.login1.Manager.Reboot(interactive=false). The
//     call returns synchronously when logind has accepted the request;
//     the actual kernel-level reboot runs asynchronously a few seconds
//     later.
//  3. POST a "restarting" intermediate state to FleetCom so the
//     dashboard reflects the in-flight reboot. The server's
//     ReconcileHostReboot flips the command to 'done' once the
//     post-reboot heartbeat arrives with a different boot_id.
//  4. Return errHandlerAlreadyReported so runAndReport doesn't post a
//     conflicting "done" — the heartbeat-based reconcile owns that.
//
// On any failure before logind accepts (no socket, polkit denial, etc.)
// the handler returns a regular error and the command is recorded as
// failed without ever bothering the kernel.
func handleHostReboot(id int64, _ json.RawMessage) (json.RawMessage, error) {
	// Connect to the system bus. With the standard bind-mount this hits
	// /var/run/dbus/system_bus_socket; godbus reads DBUS_SYSTEM_BUS_ADDRESS
	// or falls back to the well-known path.
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("connect system bus: %w (is /var/run/dbus/system_bus_socket bind-mounted into the bosun container?)", err)
	}
	// Don't conn.Close() — godbus shares the singleton; closing breaks
	// any subsequent calls in the same process. Bosun is about to be
	// killed by the reboot anyway.

	// Reboot signature: b interactive. interactive=false skips the
	// PolicyKit "may i?" prompt; uid 0 inside the container is permitted
	// by default polkit rules on every distro we run.
	obj := conn.Object("org.freedesktop.login1", dbus.ObjectPath("/org/freedesktop/login1"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call := obj.CallWithContext(ctx, "org.freedesktop.login1.Manager.Reboot", 0, false)
	if call.Err != nil {
		return nil, fmt.Errorf("logind Reboot rejected: %w", call.Err)
	}
	log.Printf("command %d: logind accepted reboot — host going down shortly", id)

	// Tell the server we're restarting. Same sentinel pattern as
	// agent.update — runAndReport sees errHandlerAlreadyReported and
	// won't post a competing 'done'. The post-reboot heartbeat with a
	// new boot_id is what flips the command to 'done'.
	serverURL := strings.TrimRight(os.Getenv("FLEETCOM_URL"), "/")
	token := os.Getenv("FLEETCOM_TOKEN")
	reportResult(serverURL, token, commandResult{
		ID:     id,
		Status: "restarting",
		Result: json.RawMessage(`{"phase":"reboot-requested","via":"systemd-logind"}`),
	})
	return nil, errHandlerAlreadyReported
}
