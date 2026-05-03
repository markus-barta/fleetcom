// Package api — onboarding state (FLEET-121).
//
// The wizard's "what does this fleet still need set up?" probe.
// Drives the first-run banner that appears above the host grid when
// any of these is non-empty:
//
//  1. Hosts with a recent bosun heartbeat but no OpenClaw gateway
//     paired yet — these are the "ready to run the pair-flow"
//     candidates.
//  2. Hosts with a paired gateway but no bridges registered — these
//     are the "ready to deploy a bridge" candidates.
//  3. Bridges in pending approval state — these are the "waiting on
//     operator eyeballs" rows that FLEET-120's verify-modal handles.
//
// `wizard_actionable` collapses the three lists to one boolean so the
// banner can render with a simple x-show. Admin-only.
package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/markus-barta/fleetcom/internal/db"
)

type onboardingHostRef struct {
	Hostname string `json:"hostname"`
	LastSeen string `json:"last_seen,omitempty"`
}

type onboardingGatewayRef struct {
	Hostname      string `json:"hostname"`
	GatewayStatus string `json:"gateway_status"`
}

type onboardingPendingRef struct {
	Host  string `json:"host"`
	Agent string `json:"agent"`
	FP    string `json:"fp"`
}

type onboardingState struct {
	HostsWithBosunNoGateway  []onboardingHostRef    `json:"hosts_with_bosun_no_gateway"`
	HostsWithGatewayNoBridge []onboardingGatewayRef `json:"hosts_with_gateway_no_bridge"`
	GatewaysPendingApproval  []onboardingPendingRef `json:"gateways_pending_approval"`
	WizardActionable         bool                   `json:"wizard_actionable"`
}

// OnboardingState handles GET /api/onboarding/state. Admin-only,
// always 200 with a fresh snapshot — operators expect the banner
// count to reflect the current state, not a cached view.
func OnboardingState(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hosts, err := store.AllHosts()
		if err != nil {
			http.Error(w, "onboarding: "+err.Error(), http.StatusInternalServerError)
			return
		}
		gws, err := store.AllGateways()
		if err != nil {
			http.Error(w, "onboarding: "+err.Error(), http.StatusInternalServerError)
			return
		}
		bridges, err := store.AllBridgePairings()
		if err != nil {
			http.Error(w, "onboarding: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Index: hostname -> gateway row (or nil).
		gwByHost := make(map[string]*db.OpenClawGateway, len(gws))
		for i := range gws {
			gwByHost[gws[i].Host] = &gws[i]
		}
		// Index: hostname -> approved-bridge count. Pending rows are
		// surfaced in the third bucket; we only count "approved" here
		// to know "this host has no live bridge yet."
		approvedBridgesByHost := make(map[string]int)
		for _, b := range bridges {
			if b.Status == "approved" {
				approvedBridgesByHost[b.Host]++
			}
		}

		state := onboardingState{
			HostsWithBosunNoGateway:  []onboardingHostRef{},
			HostsWithGatewayNoBridge: []onboardingGatewayRef{},
			GatewaysPendingApproval:  []onboardingPendingRef{},
		}

		for _, h := range hosts {
			// "Bosun seen" = host row with a non-empty last_seen.
			// Hosts without ever-seen bosun aren't actionable yet —
			// the operator's first task is bringing bosun up, which
			// happens outside FleetCom.
			if h.LastSeen == "" {
				continue
			}
			gw := gwByHost[h.Hostname]
			if gw == nil || gw.Status != "paired" {
				state.HostsWithBosunNoGateway = append(state.HostsWithBosunNoGateway, onboardingHostRef{
					Hostname: h.Hostname,
					LastSeen: h.LastSeen,
				})
				continue
			}
			// Gateway is paired — check for at least one approved bridge.
			if approvedBridgesByHost[h.Hostname] == 0 {
				state.HostsWithGatewayNoBridge = append(state.HostsWithGatewayNoBridge, onboardingGatewayRef{
					Hostname:      h.Hostname,
					GatewayStatus: gw.Status,
				})
			}
		}

		// Pending approvals — flat list across all hosts.
		for _, b := range bridges {
			if b.Status != "pending" {
				continue
			}
			state.GatewaysPendingApproval = append(state.GatewaysPendingApproval, onboardingPendingRef{
				Host:  b.Host,
				Agent: b.Agent,
				FP:    b.PubkeyFP,
			})
		}

		// Stable order makes UI deterministic across reloads.
		sort.Slice(state.HostsWithBosunNoGateway, func(i, j int) bool {
			return state.HostsWithBosunNoGateway[i].Hostname < state.HostsWithBosunNoGateway[j].Hostname
		})
		sort.Slice(state.HostsWithGatewayNoBridge, func(i, j int) bool {
			return state.HostsWithGatewayNoBridge[i].Hostname < state.HostsWithGatewayNoBridge[j].Hostname
		})
		sort.Slice(state.GatewaysPendingApproval, func(i, j int) bool {
			a, b := state.GatewaysPendingApproval[i], state.GatewaysPendingApproval[j]
			if a.Host != b.Host {
				return a.Host < b.Host
			}
			return a.Agent < b.Agent
		})

		state.WizardActionable = len(state.HostsWithBosunNoGateway) > 0 ||
			len(state.HostsWithGatewayNoBridge) > 0 ||
			len(state.GatewaysPendingApproval) > 0

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(state)
	}
}
