// Package api — gateway preflight (FLEET-118).
//
// Synchronous probe operators run before clicking "Pair gateway" so the
// wizard's step-2 surface can render an actionable checklist. Three
// independent checks:
//
//  1. Bosun freshness — has the host heartbeated within the last 120s?
//     If not, the operator is about to enqueue a command nothing will
//     pull for at least a minute.
//  2. Gateway port reachable — TCP-dial wss://<host>:18789 with 3s
//     timeout. Catches "OpenClaw isn't running."
//  3. TLS handshake — full crypto/tls negotiation against the host's
//     name. Catches "OpenClaw is up but its cert doesn't match."
//
// Each check that fails appends a stable identifier to .blockers so the
// frontend can switch on the code and show the right hint copy. Ready
// is the AND of all three.
//
// Deliberately *not* cached — operators expect a fresh probe each time
// they open the pair-flow modal, and the round-trip is bounded by the
// 3s dial timeout.
package api

import (
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/markus-barta/fleetcom/internal/db"
)

const (
	// gatewayPort is OpenClaw's default WSS listener. Hardcoded for v1
	// to match the bosun side; promote to a per-gateway column if/when
	// non-standard ports become a real need.
	gatewayPort = 18789

	// bosunFreshSeconds is the threshold for "has the host checked in
	// recently enough that enqueueing a command will actually be picked
	// up." The bosun heartbeat is 60s, so 120s allows for one missed
	// beat without flagging a blocker.
	bosunFreshSeconds = 120

	// preflightProbeTimeout caps both the TCP dial and the TLS
	// handshake. Operators feel the cumulative wait, so keep it tight.
	preflightProbeTimeout = 3 * time.Second
)

// preflightResult is the JSON contract documented in FLEET-118.
type preflightResult struct {
	Host                 string   `json:"host"`
	BosunSeenAt          string   `json:"bosun_seen_at,omitempty"`
	BosunSeenAgoSeconds  *int64   `json:"bosun_seen_ago_seconds,omitempty"`
	GatewayPort          int      `json:"gateway_port"`
	GatewayPortReachable bool     `json:"gateway_port_reachable"`
	TLSOK                bool     `json:"tls_ok"`
	TLSError             string   `json:"tls_error,omitempty"`
	Ready                bool     `json:"ready"`
	Blockers             []string `json:"blockers"`
}

// Stable blocker codes — keep these stable forever; the frontend
// switches on them to render hint copy.
const (
	blockerBosunNeverSeen   = "bosun_never_seen"
	blockerBosunStale       = "bosun_stale"
	blockerHostUnknown      = "host_unknown"
	blockerGatewayUnreach   = "gateway_unreachable"
	blockerTLSFailed        = "tls_failed"
	blockerAlreadyPairedKey = "already_paired" // reserved for future use
)

// GatewayPreflight runs the three probes against the named host and
// returns the result as JSON. Always returns 200 — even when blockers
// are present, the body is the source of truth.
func GatewayPreflight(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := chi.URLParam(r, "host")
		if host == "" {
			http.Error(w, "host required", http.StatusBadRequest)
			return
		}
		result := preflightResult{
			Host:        host,
			GatewayPort: gatewayPort,
			Blockers:    []string{},
		}

		// 1. Bosun freshness from the hosts table.
		var lastSeen string
		err := store.DB.QueryRow(
			`SELECT last_seen FROM hosts WHERE hostname = ? LIMIT 1`,
			host,
		).Scan(&lastSeen)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			result.Blockers = append(result.Blockers, blockerHostUnknown)
		case err != nil:
			http.Error(w, "preflight: "+err.Error(), http.StatusInternalServerError)
			return
		case lastSeen == "":
			result.Blockers = append(result.Blockers, blockerBosunNeverSeen)
		default:
			t, parseErr := time.Parse(time.RFC3339, lastSeen)
			if parseErr != nil || t.IsZero() {
				result.Blockers = append(result.Blockers, blockerBosunNeverSeen)
			} else {
				result.BosunSeenAt = t.UTC().Format(time.RFC3339)
				ago := int64(time.Since(t).Seconds())
				if ago < 0 {
					ago = 0 // clock skew safety
				}
				result.BosunSeenAgoSeconds = &ago
				if ago > bosunFreshSeconds {
					result.Blockers = append(result.Blockers, blockerBosunStale)
				}
			}
		}

		// 2 + 3. TCP dial then TLS handshake on the same connection.
		// Skip both if the host row is unknown — there's no DNS to
		// resolve in a meaningful way. (We *could* still try, but the
		// operator's first action should be "register the host," not
		// "stare at a connection error.")
		//
		// QA-AUDIT-FIX (PPM 1527): upgrade the existing TCP conn into
		// TLS via tls.Client(conn, cfg).Handshake instead of dialing
		// twice. Saves one round-trip + halves the worst-case wait if
		// the host is slow but reachable. The TCP-level success is
		// observed by completion of DialTimeout; "TLS broken on top of
		// TCP" is then a clean second class of error.
		if !contains(result.Blockers, blockerHostUnknown) {
			// JoinHostPort handles IPv6 bracketing (`[::1]:18789`) — vet
			// flags the naive Sprintf %s:%d form for this reason.
			addr := net.JoinHostPort(host, strconv.Itoa(gatewayPort))

			tcpConn, dialErr := net.DialTimeout("tcp", addr, preflightProbeTimeout)
			if dialErr != nil {
				result.Blockers = append(result.Blockers, blockerGatewayUnreach)
			} else {
				result.GatewayPortReachable = true
				// Set a deadline on the TLS handshake on the existing
				// conn so a misbehaving peer can't hold us beyond the
				// caller's tolerance.
				_ = tcpConn.SetDeadline(time.Now().Add(preflightProbeTimeout))
				tlsConn := tls.Client(tcpConn, &tls.Config{
					ServerName: host,
					MinVersion: tls.VersionTLS12,
				})
				if tlsErr := tlsConn.Handshake(); tlsErr != nil {
					result.TLSError = tlsErr.Error()
					result.Blockers = append(result.Blockers, blockerTLSFailed)
				} else {
					result.TLSOK = true
				}
				_ = tlsConn.Close() // also closes the underlying TCP conn
			}
		}

		result.Ready = len(result.Blockers) == 0

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
