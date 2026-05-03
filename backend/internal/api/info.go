package api

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/version"
)

// FLEET-80: GET /api/info — self-describing API catalog.
//
// A fresh agent encountering FleetCom for the first time should be able
// to learn the API surface in a single curl, without grepping source.
// The catalog is hand-maintained (one place, this file) and kept honest
// by TestInfoCatalog (info_test.go) which walks the live chi router and
// asserts every registered route is either in EndpointCatalog or in
// ExcludedFromInfo with a documented reason.
//
// JSON shape is treated as a public contract: adding fields is allowed,
// renaming/removing existing ones is a breaking change.

// AuthMethod describes one way to authenticate against the API.
type AuthMethod struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes,omitempty"`
}

// EndpointInfo describes one route in the catalog. Auth lists every
// auth method that can call this route — e.g. ["session","api_token"]
// for the FLEET-79 read endpoints, ["agent_bearer"] for bosun POSTs,
// ["public"] for unauthenticated. Scope is only meaningful for
// api_token-callable routes.
type EndpointInfo struct {
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Auth          []string `json:"auth"`
	Scope         string   `json:"scope,omitempty"`
	RequiresAdmin bool     `json:"requires_admin,omitempty"`
	Description   string   `json:"description"`
}

// InfoResponse is the wire shape of GET /api/info.
type InfoResponse struct {
	Version     string            `json:"version"`
	Commit      string            `json:"commit"`
	BuildTime   string            `json:"build_time"`
	AuthMethods []AuthMethod      `json:"auth_methods"`
	Endpoints   []EndpointInfo    `json:"endpoints"`
	Links       map[string]string `json:"links"`
}

// AuthMethodsCatalog enumerates the four auth flows the server supports.
// Exported so tests and downstream consumers can reference the canonical
// list without re-deriving it from the source.
var AuthMethodsCatalog = []AuthMethod{
	{
		Type:        "public",
		Description: "No authentication required.",
	},
	{
		Type:        "session",
		Description: "Browser cookie (HttpOnly fleetcom_session) + mandatory TOTP via /login.",
	},
	{
		Type:        "api_token",
		Description: "Bearer fleet_pat_<64hex> (FLEET-79). Self-service issuance at /api/auth/api-tokens. Inherits the owning user's host-access scope.",
		Scopes:      auth.APITokenScopes,
	},
	{
		Type:        "agent_bearer",
		Description: "Per-host bearer token used by Bosun for write-side endpoints (heartbeat, container-events, agent-events, bridge registration, command results).",
	},
	{
		Type:        "share_token",
		Description: "Read-only share-link token embedded in the URL path (/s/{token}). Inherits the link creator's host-access scope.",
	},
}

// EndpointCatalog is the single source of truth for the public API
// surface. Entries are grouped by area for readability; the JSON output
// preserves this order (no sorting). When you add a route in
// cmd/server/router.go, add a matching entry here — the drift test will
// fail otherwise.
var EndpointCatalog = []EndpointInfo{
	// ---------- Public, unauthenticated ----------
	{Method: "GET", Path: "/api/info", Auth: []string{"public"},
		Description: "This catalog. Always public; safe to call without credentials."},
	{Method: "GET", Path: "/api/version", Auth: []string{"public"},
		Description: "Build version, commit, and feature flags (e.g. destructive_commands_enabled)."},
	{Method: "GET", Path: "/api/settings", Auth: []string{"public"},
		Description: "Public-safe settings (heartbeat interval, branding label, instance domain)."},
	{Method: "GET", Path: "/api/org-logo", Auth: []string{"public"},
		Description: "Org logo bytes (PNG). Used by the dashboard header before login."},
	{Method: "GET", Path: "/api/image-presets/{id}/image", Auth: []string{"public"},
		Description: "Host-icon image bytes by preset id."},

	// ---------- Bosun write-side (per-host bearer) ----------
	{Method: "POST", Path: "/api/heartbeat", Auth: []string{"agent_bearer"},
		Description: "Periodic enriched heartbeat: hosts, containers, agents, deployment_shape, boot_id."},
	{Method: "POST", Path: "/api/container-events", Auth: []string{"agent_bearer"},
		Description: "Real-time container lifecycle events from Docker socket (die, restart, oom, health_status)."},
	{Method: "POST", Path: "/api/agent-events", Auth: []string{"agent_bearer"},
		Description: "Agent observability stream: turn started/finished, tool invocations, errors."},
	{Method: "POST", Path: "/api/bridges/register", Auth: []string{"agent_bearer"},
		Description: "Agent-bridge announces itself and its public key for OpenClaw pairing."},
	{Method: "POST", Path: "/api/command-results", Auth: []string{"agent_bearer"},
		Description: "Bosun reports the result of a command pulled from /api/heartbeat."},

	// ---------- Share links (read-only) ----------
	{Method: "GET", Path: "/s/{token}", Auth: []string{"share_token"},
		Description: "Read-only dashboard view scoped to the share-link creator's hosts."},
	{Method: "GET", Path: "/s/{token}/events", Auth: []string{"share_token"},
		Description: "SSE stream for the share-linked dashboard."},

	// ---------- Token-callable read endpoints (FLEET-79) ----------
	{Method: "GET", Path: "/api/hosts", Auth: []string{"session", "api_token"}, Scope: "read:hosts",
		Description: "List hosts the caller has access to, with containers, agents, and live status."},
	{Method: "GET", Path: "/api/hosts/{hostname}/hardware", Auth: []string{"session", "api_token"}, Scope: "read:hardware",
		Description: "Hardware snapshot + recent live metrics for a single host."},
	{Method: "GET", Path: "/api/agents", Auth: []string{"session", "api_token"}, Scope: "read:agents",
		Description: "List agents across accessible hosts (FLEET-36 observability)."},
	{Method: "GET", Path: "/api/agents/{host}/{name}", Auth: []string{"session", "api_token"}, Scope: "read:agents",
		Description: "Single agent detail with recent turns and tool invocations."},

	// ---------- Browser session, regular user ----------
	{Method: "GET", Path: "/api/me", Auth: []string{"session"},
		Description: "The authenticated user (id, email, role, totp_enabled, avatar)."},
	{Method: "GET", Path: "/api/events", Auth: []string{"session"},
		Description: "SSE stream of dashboard mutations, scoped to the user's hosts."},
	{Method: "GET", Path: "/api/history", Auth: []string{"session"},
		Description: "Status-sample history for the host grid (rolling 1y window)."},
	{Method: "GET", Path: "/api/ignored", Auth: []string{"session"},
		Description: "Per-user list of ignored hosts/containers/agents."},
	{Method: "POST", Path: "/api/ignore", Auth: []string{"session"},
		Description: "Add an entity to the user's ignore list."},
	{Method: "DELETE", Path: "/api/ignore", Auth: []string{"session"},
		Description: "Remove an entity from the user's ignore list."},
	{Method: "GET", Path: "/api/host-configs", Auth: []string{"session"},
		Description: "Per-host UI config (icon preset, comment) for accessible hosts."},
	{Method: "GET", Path: "/api/image-presets", Auth: []string{"session"},
		Description: "List host-icon presets (metadata only; bytes via /api/image-presets/{id}/image)."},

	// ---------- Self-service auth ----------
	{Method: "POST", Path: "/api/auth/password", Auth: []string{"session"},
		Description: "Change the caller's password (kills other sessions)."},
	{Method: "GET", Path: "/api/auth/totp/setup", Auth: []string{"session"},
		Description: "Generate a new TOTP secret + QR for the caller."},
	{Method: "POST", Path: "/api/auth/totp/enable", Auth: []string{"session"},
		Description: "Enable TOTP after verifying a code from the secret returned by /setup."},
	{Method: "POST", Path: "/api/auth/totp/disable", Auth: []string{"session"},
		Description: "Disable TOTP (requires password)."},
	{Method: "GET", Path: "/api/auth/sessions", Auth: []string{"session"},
		Description: "List the caller's active browser sessions."},
	{Method: "DELETE", Path: "/api/auth/sessions/{id}", Auth: []string{"session"},
		Description: "Revoke one of the caller's browser sessions."},
	{Method: "POST", Path: "/api/auth/avatar", Auth: []string{"session"},
		Description: "Upload a profile picture (data URL, 128x128 JPEG, ~10KB)."},
	{Method: "DELETE", Path: "/api/auth/avatar", Auth: []string{"session"},
		Description: "Remove the caller's profile picture."},
	{Method: "GET", Path: "/api/auth/api-tokens", Auth: []string{"session"},
		Description: "List the caller's fleet_pat_ tokens (FLEET-79). Hashes never returned."},
	{Method: "POST", Path: "/api/auth/api-tokens", Auth: []string{"session"},
		Description: "Mint a new fleet_pat_ token. Plaintext value returned ONCE in the response."},
	{Method: "DELETE", Path: "/api/auth/api-tokens/{id}", Auth: []string{"session"},
		Description: "Revoke one of the caller's fleet_pat_ tokens (sticky, immediate)."},

	// ---------- FLEET-108: operator activity log ----------
	{Method: "GET", Path: "/api/activity", Auth: []string{"session"},
		Description: "Operator activity log. Admins see all rows; regular users see only their own. Filters: since (RFC3339), limit, verb, target, outcome."},
	{Method: "POST", Path: "/api/activity", Auth: []string{"session"},
		Description: "Record one activity row. Called by busy() in the browser after every async user-initiated action."},

	// ---------- Admin: gateways / bridges (OpenClaw) ----------
	{Method: "GET", Path: "/api/gateways", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "All paired OpenClaw gateways and their per-host status."},
	{Method: "GET", Path: "/api/gateways/pairable-hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Hosts that have an OpenClaw process running but no FleetCom pairing yet."},
	{Method: "POST", Path: "/api/gateways/{host}/pair", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Generate a fresh keypair and enqueue an openclaw.pair command on the host."},
	{Method: "DELETE", Path: "/api/gateways/{host}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Unpair an OpenClaw gateway and delete its on-disk keypair."},
	{Method: "POST", Path: "/api/gateways/{host}/auto-approve/{mode}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Toggle whether new bridge registrations from this gateway are auto-approved."},
	{Method: "GET", Path: "/api/bridges", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "All registered agent-bridges."},
	{Method: "DELETE", Path: "/api/bridges/{host}/{agent}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Revoke a bridge pairing."},

	// ---------- Admin: bosun command channel (FLEET-60) ----------
	{Method: "POST", Path: "/api/hosts/{host}/commands", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Enqueue a command for bosun on this host (e.g. host.reboot, agent.update)."},
	{Method: "GET", Path: "/api/hosts/{host}/commands", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "List recent command runs for this host."},
	{Method: "POST", Path: "/api/commands/{id}/cancel", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Cancel a pending command (no effect once executing)."},

	// ---------- Admin: hosts ----------
	{Method: "GET", Path: "/api/hosts/all", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "All hosts, ignoring per-user host-access (admin-wide audit view)."},
	{Method: "POST", Path: "/api/hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Add a new host. Returns a per-host bearer token (shown ONCE) for bosun."},
	{Method: "DELETE", Path: "/api/hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Delete a host and cascade its containers/agents/metrics."},
	{Method: "POST", Path: "/api/hosts/{hostname}/update", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Trigger an on-host bosun update (universal updater dispatches per deployment_shape)."},
	{Method: "POST", Path: "/api/hosts/{hostname}/token", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Regenerate the host's bearer token (rotation; old token invalidates)."},
	{Method: "PUT", Path: "/api/hosts/{hostname}/allow-reboot", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Per-host kill switch for the destructive host.reboot command (FLEET-369.1)."},
	{Method: "POST", Path: "/api/hosts/update-all", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Fan out the update command to every host."},

	// ---------- Admin: settings & misc ----------
	{Method: "GET", Path: "/api/tokens", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "List host bearer tokens (metadata only; hashes never returned)."},
	{Method: "GET", Path: "/api/shares", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "List share links (read-only dashboard URLs)."},
	{Method: "POST", Path: "/api/shares", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Create a share link. Returns the token in the response."},
	{Method: "DELETE", Path: "/api/shares", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Delete a share link."},
	{Method: "PUT", Path: "/api/settings", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Update server settings (heartbeat interval, branding, etc.)."},
	{Method: "PUT", Path: "/api/host-config", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Update per-host UI config (icon preset, comment)."},
	{Method: "POST", Path: "/api/image-presets", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Upload a new host-icon preset."},
	{Method: "DELETE", Path: "/api/image-presets/{id}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Delete a host-icon preset."},
	{Method: "POST", Path: "/api/image-presets/import", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Bulk import host-icon presets from a ZIP."},
	{Method: "GET", Path: "/api/image-presets/export", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Export all host-icon presets as a ZIP bundle."},
	{Method: "POST", Path: "/api/org-logo", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Set the org logo (data URL bytes stored in settings)."},
	{Method: "DELETE", Path: "/api/org-logo", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Remove the org logo."},

	// ---------- Admin: users ----------
	{Method: "GET", Path: "/api/users", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "List users (excludes deleted)."},
	{Method: "POST", Path: "/api/users", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Create a new user (admin or regular)."},
	{Method: "DELETE", Path: "/api/users/{id}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Soft-delete a user (status='deleted', sessions invalidated)."},
	{Method: "PUT", Path: "/api/users/{id}/status", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Set a user's status (active/inactive)."},
	{Method: "POST", Path: "/api/users/{id}/password", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Admin-set a user's password (kills their sessions)."},
	{Method: "POST", Path: "/api/users/{id}/reset-totp", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Reset a user's TOTP (forces re-enrollment on next login)."},
	{Method: "DELETE", Path: "/api/users/{id}/sessions", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Invalidate all of a user's sessions (force re-login)."},
	{Method: "GET", Path: "/api/users/{id}/hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "List the hosts a user has access to."},
	{Method: "POST", Path: "/api/users/{id}/hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Grant a user access to one host."},
	{Method: "POST", Path: "/api/users/{id}/hosts/grant-all", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Grant a user access to all hosts."},
	{Method: "DELETE", Path: "/api/users/{id}/hosts", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Revoke all of a user's host access."},
	{Method: "DELETE", Path: "/api/users/{id}/hosts/{hostId}", Auth: []string{"session"}, RequiresAdmin: true,
		Description: "Revoke a user's access to one host."},
}

// ExcludedFromInfo names routes that intentionally do NOT appear in
// /api/info. Each entry is keyed "METHOD path" and MUST have a comment
// explaining why the omission is correct. The drift test allows these
// keys to exist in the live router without a catalog entry.
var ExcludedFromInfo = map[string]string{
	// Liveness probes for load balancers / nginx — ops concern, not API.
	"GET /healthz": "liveness probe; documented in DEPLOYMENT.md, not part of the API contract",
	// LICENSE text — not an API.
	"GET /LICENSE": "static text file; not API",
	// Static asset server — implementation detail of the SPA shell.
	// chi.Handle() registers under every HTTP method, so we exclude all of
	// them rather than just GET.
	"GET /static/*":     "SPA static assets; not API",
	"HEAD /static/*":    "SPA static assets; not API",
	"POST /static/*":    "SPA static assets; not API",
	"PUT /static/*":     "SPA static assets; not API",
	"DELETE /static/*":  "SPA static assets; not API",
	"PATCH /static/*":   "SPA static assets; not API",
	"OPTIONS /static/*": "SPA static assets; not API",
	"CONNECT /static/*": "SPA static assets; not API",
	"TRACE /static/*":   "SPA static assets; not API",
	// HTML pages and form handlers for the browser-only login flow. They
	// serve text/html, not JSON, and are discoverable via redirect from
	// any protected route. Documenting them as "endpoints" misrepresents
	// them as machine-callable.
	"GET /":                 "dashboard SPA shell (text/html)",
	"GET /login":            "login form (text/html)",
	"POST /login":           "login form submit (form-urlencoded, returns redirect or HTML)",
	"POST /login/totp":      "TOTP step submit (form-urlencoded)",
	"GET /logout":           "logout link (returns redirect)",
	"POST /forgot-password": "password reset form submit (form-urlencoded)",
	"GET /reset/{token}":    "password reset form (text/html)",
	"POST /reset":           "password reset submit (form-urlencoded)",
	"GET /setup-totp":       "mandatory TOTP setup form (text/html)",
	"POST /setup-totp":      "mandatory TOTP setup submit (form-urlencoded)",
}

// infoLinks returns the dashboard / docs / repo link map. APP_BASE_URL
// drives the dashboard URL so localhost dev and prod both render
// correctly.
func infoLinks() map[string]string {
	base := os.Getenv("APP_BASE_URL")
	if base == "" {
		base = "http://localhost:8090"
	}
	return map[string]string{
		"dashboard": base + "/",
		"repo":      "https://github.com/markus-barta/fleetcom",
		"agents_md": "https://github.com/markus-barta/fleetcom/blob/main/AGENTS.md",
		"docs":      "https://github.com/markus-barta/fleetcom/tree/main/docs",
	}
}

// Info handles GET /api/info. Public, no auth, no caching.
func Info(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(InfoResponse{
		Version:     version.Version,
		Commit:      version.Commit,
		BuildTime:   version.BuildTime,
		AuthMethods: AuthMethodsCatalog,
		Endpoints:   EndpointCatalog,
		Links:       infoLinks(),
	})
}
