package main

// Mutation → SSE event mapping (FLEET-71).
// Every server-side state change that's visible in the dashboard pushes a
// named SSE event; the browser binds reactively to those data fields, so
// mutations reflect within ~1s without a manual reload.
//
//   POST /api/heartbeat                           → "hosts" + optional "agents"
//   POST /api/container-events                    → "hosts"
//   PUT  /api/host-config                         → "host-configs"
//   PUT  /api/settings, POST /api/branding/*      → "config"
//   POST /api/hosts/{host}/commands               → (no broadcast — pull-based)
//   POST /api/command-results                     → "commands"
//   POST /api/commands/{id}/cancel                → "commands"
//   POST /api/gateways/{host}/pair                → "gateways"
//   DELETE /api/gateways/{host}                   → "gateways"
//   POST /api/gateways/{host}/auto-approve/{mode} → "gateways"
//   DELETE /api/bridges/{host}/{agent}            → "bridges" + "gateways"
//   POST /api/update-all, POST /api/hosts/{h}/update → "hosts"
//   (agent observability push)                    → "agents" + "agent-event"

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/markus-barta/fleetcom/internal/api"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/openclaw"
	"github.com/markus-barta/fleetcom/internal/sse"
)

// routerDeps bundles the dependencies the router needs. Extracted into
// a struct so the FLEET-80 drift test can build a router without
// duplicating route registration.
type routerDeps struct {
	store         *db.Store
	hub           *sse.Hub
	auth          *auth.Auth
	resetHandlers *auth.ResetHandlers
	ocMgr         *openclaw.Manager
}

// newRouter builds the configured chi router. Same wiring as the
// previous inline setup; the only behavior change is that the routes
// table is now reachable from the test binary.
func newRouter(d *routerDeps) chi.Router {
	store := d.store
	hub := d.hub
	a := d.auth
	resetHandlers := d.resetHandlers
	ocMgr := d.ocMgr

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Public routes
	r.Get("/healthz", api.Healthz)
	r.Get("/api/version", api.Version)
	// FLEET-80: self-describing API catalog for agent onboarding.
	r.Get("/api/info", api.Info)
	r.Get("/api/settings", api.GetSettings(store))
	r.Get("/api/image-presets/{id}/image", api.GetImagePresetImage(store))
	r.Get("/api/org-logo", api.GetOrgLogo(store))
	r.Get("/LICENSE", api.License)
	r.Post("/api/heartbeat", api.Heartbeat(store, hub))
	r.Post("/api/container-events", api.ContainerEvents(store, hub))
	r.Post("/api/agent-events", api.AgentEvents(store, hub))
	// FLEET-51 + FLEET-113: bridge registration. Bosun-bearer-authenticated,
	// public endpoint. ocMgr is passed for the OOB confirmation-code push
	// when the host's gateway has oob_delivery_enabled=ON.
	r.Post("/api/bridges/register", api.RegisterBridge(store, hub, ocMgr))
	// FLEET-60: bosun reports command results here (bosun-bearer auth).
	r.Post("/api/command-results", api.CommandResults(store, hub))

	// Auth routes (public)
	r.Get("/login", api.LoginPage)
	r.Post("/login", a.HandleLogin)
	r.Post("/login/totp", a.HandleTOTPVerify)
	r.Get("/logout", a.HandleLogout)
	r.Post("/forgot-password", resetHandlers.HandleForgotPassword)
	r.Get("/reset/{token}", resetHandlers.HandleResetForm)
	r.Post("/reset", resetHandlers.HandleResetPassword)

	// Static files (public — CSS, JS, images)
	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))

	// Share links (read-only, token-authenticated)
	r.Get("/s/{token}", api.SharedDashboard(store))
	r.Get("/s/{token}/events", api.SharedEvents(store, hub))

	// TOTP setup (session required, but exempt from TOTP enforcement)
	r.Group(func(r chi.Router) {
		r.Use(a.RequireSession)
		r.Get("/setup-totp", a.HandleSetupTOTP)
		r.Post("/setup-totp", a.HandleSetupTOTPSubmit)
		r.Get("/api/me", api.Me(store))
	})

	// FLEET-79: read-only endpoints that accept either a session cookie OR
	// a "fleet_pat_…" bearer token with the matching scope. These live
	// outside the standard r.Group() because MaybeAPIToken must run BEFORE
	// RequireSession (chi's group .Use middleware runs ahead of any
	// per-route .With middleware, which would defeat the short-circuit).
	// On token auth, MaybeAPIToken sets a context flag that
	// RequireSession+RequireTOTP honor and skip past.
	tokenOrSession := func(scope string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return auth.MaybeAPIToken(store, scope)(a.RequireSession(auth.RequireTOTP(next)))
		}
	}
	r.With(tokenOrSession("read:hosts")).Get("/api/hosts", api.ListHosts(store))
	r.With(tokenOrSession("read:hardware")).Get("/api/hosts/{hostname}/hardware", api.HostHardware(store))
	r.With(tokenOrSession("read:agents")).Get("/api/agents", api.ListAgents(store))
	r.With(tokenOrSession("read:agents")).Get("/api/agents/{host}/{name}", api.AgentDetail(store))

	// Protected routes (session + TOTP required)
	r.Group(func(r chi.Router) {
		r.Use(a.RequireSession)
		r.Use(auth.RequireTOTP)
		r.Get("/", api.Dashboard)
		r.Get("/api/events", api.Events(store, hub))
		// FLEET-51: OpenClaw gateway pairing + bridge registry.
		r.With(auth.RequireAdmin).Get("/api/gateways", api.ListGateways(store))
		r.With(auth.RequireAdmin).Get("/api/gateways/pairable-hosts", api.HostsAvailableForPairing(store))
		// FLEET-61: wizard-style gateway pairing. Generates keys + enqueues
		// openclaw.pair command. First dir of ocKeyDir (colon-separated)
		// that's writable is used for key storage; /app/data is the
		// conventional choice inside the fleetcom container.
		r.With(auth.RequireAdmin).Post("/api/gateways/{host}/pair", api.PairGateway(store, "/app/data/openclaw-keys", hub, ocMgr))
		r.With(auth.RequireAdmin).Delete("/api/gateways/{host}", api.UnpairGateway(store, "/app/data/openclaw-keys", hub, ocMgr))
		r.With(auth.RequireAdmin).Post("/api/gateways/{host}/auto-approve/{mode}", api.SetGatewayAutoApprove(store, hub))
		r.With(auth.RequireAdmin).Get("/api/bridges", api.ListBridges(store))
		r.With(auth.RequireAdmin).Delete("/api/bridges/{host}/{agent}", api.RevokeBridge(store, hub, ocMgr))
		// FLEET-109: smart-suggestion chip rails for the bridge-deploy modal.
		r.With(auth.RequireAdmin).Get("/api/bridges/suggestions/{host}", api.BridgeSuggestions(store))
		// FLEET-112: pair-request approval surface.
		r.With(auth.RequireAdmin).Get("/api/bridges/pending", api.ListPendingBridges(store))
		r.With(auth.RequireAdmin).Post("/api/bridges/{host}/{agent}/approve", api.ApproveBridge(store, hub))
		r.With(auth.RequireAdmin).Post("/api/bridges/{host}/{agent}/reject", api.RejectBridge(store, hub))
		// FLEET-113: OOB confirmation-code path + per-gateway toggle.
		r.With(auth.RequireAdmin).Post("/api/bridges/{host}/{agent}/approve-skip-oob", api.ApproveBridgeSkipOOB(store, hub))
		r.With(auth.RequireAdmin).Post("/api/gateways/{host}/oob-delivery/{mode}", api.SetGatewayOOBDelivery(store, hub))
		// FLEET-114: per-gateway attestation toggle + operator-paste pubkey.
		r.With(auth.RequireAdmin).Post("/api/gateways/{host}/attestation/{mode}", api.SetGatewayAttestationRequired(store, hub))
		r.With(auth.RequireAdmin).Put("/api/gateways/{host}/pubkey", api.SetGatewayPubkey(store, hub))

		// FLEET-60: bosun command channel — admin issues work, bosun
		// picks it up via heartbeat response, reports back here.
		r.With(auth.RequireAdmin).Post("/api/hosts/{host}/commands", api.EnqueueCommand(store))
		r.With(auth.RequireAdmin).Get("/api/hosts/{host}/commands", api.ListCommands(store))
		r.With(auth.RequireAdmin).Post("/api/commands/{id}/cancel", api.CancelCommand(store))
		r.Get("/api/history", api.History(store))
		r.Get("/api/ignored", api.ListIgnored(store))
		r.Post("/api/ignore", api.AddIgnore(store))
		r.Delete("/api/ignore", api.RemoveIgnore(store))
		r.Get("/api/host-configs", api.GetHostConfigs(store))
		r.Get("/api/image-presets", api.ListImagePresets(store))
		r.Get("/api/image-presets/export", api.ExportImagePresets(store))

		// Admin-only: fleet mutations + sensitive reads
		r.With(auth.RequireAdmin).Get("/api/tokens", api.ListTokens(store))
		r.With(auth.RequireAdmin).Post("/api/hosts", api.AddHost(store))
		r.With(auth.RequireAdmin).Delete("/api/hosts", api.DeleteHost(store))
		r.With(auth.RequireAdmin).Post("/api/hosts/{hostname}/update", api.RequestHostUpdate(store, hub))
		r.With(auth.RequireAdmin).Post("/api/hosts/{hostname}/token", api.RegenerateHostToken(store))
		// FLEET-369.1 — per-host kill switch for the destructive host.reboot command.
		r.With(auth.RequireAdmin).Put("/api/hosts/{hostname}/allow-reboot", api.SetAllowReboot(store))
		r.With(auth.RequireAdmin).Post("/api/hosts/update-all", api.RequestUpdateAll(store, hub))
		r.With(auth.RequireAdmin).Get("/api/shares", api.ListShareLinks(store))
		r.With(auth.RequireAdmin).Post("/api/shares", api.CreateShareLink(store))
		r.With(auth.RequireAdmin).Delete("/api/shares", api.DeleteShareLink(store))
		r.With(auth.RequireAdmin).Put("/api/settings", api.UpdateSettings(store, hub))
		r.With(auth.RequireAdmin).Put("/api/host-config", api.UpdateHostConfig(store, hub))
		r.With(auth.RequireAdmin).Post("/api/image-presets", api.UploadImagePreset(store))
		r.With(auth.RequireAdmin).Delete("/api/image-presets/{id}", api.DeleteImagePreset(store))
		r.With(auth.RequireAdmin).Post("/api/image-presets/import", api.ImportImagePresets(store))

		// Self-service auth endpoints
		r.Post("/api/auth/password", api.ChangePassword(store))
		r.Get("/api/auth/totp/setup", api.TOTPSetup(store))
		r.Post("/api/auth/totp/enable", api.TOTPEnable(store))
		r.Post("/api/auth/totp/disable", api.TOTPDisable(store))
		r.Get("/api/auth/sessions", api.ListSessions(store))
		r.Delete("/api/auth/sessions/{id}", api.RevokeSession(store))
		r.Post("/api/auth/avatar", api.UpdateAvatar(store))
		r.Delete("/api/auth/avatar", api.DeleteAvatar(store))
		// FLEET-79: user-issued read-only API tokens.
		r.Get("/api/auth/api-tokens", api.ListAPITokens(store))
		r.Post("/api/auth/api-tokens", api.CreateAPIToken(store))
		r.Delete("/api/auth/api-tokens/{id}", api.RevokeAPIToken(store))

		// FLEET-108: operator activity log. busy() in the browser POSTs
		// after every async user-initiated action; the drawer reads via GET.
		r.Get("/api/activity", api.ListActivity(store))
		r.Post("/api/activity", api.RecordActivity(store))

		// Admin-only routes
		r.Route("/api/users", func(r chi.Router) {
			r.Use(auth.RequireAdmin)
			r.Get("/", api.ListUsers(store))
			r.Post("/", api.CreateUser(store))
			r.Delete("/{id}", api.DeleteUser(store))
			r.Put("/{id}/status", api.UpdateUserStatus(store))
			r.Post("/{id}/password", api.AdminSetUserPassword(store))
			r.Post("/{id}/reset-totp", api.ResetUserTOTP(store))
			r.Delete("/{id}/sessions", api.InvalidateUserSessions(store))
			r.Get("/{id}/hosts", api.ListUserHosts(store))
			r.Post("/{id}/hosts", api.GrantUserHost(store))
			r.Post("/{id}/hosts/grant-all", api.GrantAllUserHosts(store))
			r.Delete("/{id}/hosts", api.RevokeAllUserHosts(store))
			r.Delete("/{id}/hosts/{hostId}", api.RevokeUserHost(store))
		})
		r.With(auth.RequireAdmin).Get("/api/hosts/all", api.AllHostsList(store))
		r.With(auth.RequireAdmin).Post("/api/org-logo", api.UploadOrgLogo(store, hub))
		r.With(auth.RequireAdmin).Delete("/api/org-logo", api.DeleteOrgLogo(store, hub))

	})

	return r
}
