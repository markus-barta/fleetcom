package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/markus-barta/fleetcom/internal/api"
	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/sse"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "fleetcom.db"
	}

	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Seed admin user on first run
	if err := auth.SeedAdmin(store, os.Getenv("FLEETCOM_ADMIN_EMAIL"), os.Getenv("FLEETCOM_ADMIN_PASSWORD")); err != nil {
		log.Fatalf("failed to seed admin: %v", err)
	}

	hub := sse.NewHub()
	a := auth.New(store)
	resetHandlers := auth.NewResetHandlers(store)

	// Purge samples older than 400 days (covers the 1Y history scale).
	const sampleRetention = 400 * 24 * time.Hour
	// Keep container events for 24 hours (only needed for crash loop detection).
	const eventRetention = 24 * time.Hour
	// Keep host hardware metrics for 24h (sparklines are a rolling 1h window;
	// 24h gives headroom and is still tiny — ~1440 rows/host).
	const hostMetricsRetention = 24 * time.Hour
	if n, err := store.PurgeOldSamples(sampleRetention); err != nil {
		log.Printf("initial sample purge failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old status samples", n)
	}
	if n, err := store.PurgeOldContainerEvents(eventRetention); err != nil {
		log.Printf("initial container event purge failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old container events", n)
	}
	if n, err := store.PruneOldHostMetrics(hostMetricsRetention); err != nil {
		log.Printf("initial host_metrics purge failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old host metrics", n)
	}
	store.CleanExpiredSessions()
	store.CleanExpiredTOTPPending()

	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if n, err := store.PurgeOldSamples(sampleRetention); err != nil {
				log.Printf("sample purge failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old status samples", n)
			}
			if n, err := store.PurgeOldContainerEvents(eventRetention); err != nil {
				log.Printf("container event purge failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old container events", n)
			}
			if n, err := store.PruneOldHostMetrics(hostMetricsRetention); err != nil {
				log.Printf("host_metrics purge failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old host metrics", n)
			}
			store.CleanExpiredSessions()
			store.CleanExpiredTOTPPending()
		}
	}()

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Public routes
	r.Get("/healthz", api.Healthz)
	r.Get("/api/version", api.Version)
	r.Get("/api/settings", api.GetSettings(store))
	r.Get("/api/image-presets/{id}/image", api.GetImagePresetImage(store))
	r.Get("/api/org-logo", api.GetOrgLogo(store))
	r.Get("/LICENSE", api.License)
	r.Post("/api/heartbeat", api.Heartbeat(store, hub))
	r.Post("/api/container-events", api.ContainerEvents(store, hub))

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

	// Protected routes (session + TOTP required)
	r.Group(func(r chi.Router) {
		r.Use(a.RequireSession)
		r.Use(auth.RequireTOTP)
		r.Get("/", api.Dashboard)
		r.Get("/api/events", api.Events(store, hub))
		r.Get("/api/hosts", api.ListHosts(store))
		r.Get("/api/hosts/{hostname}/hardware", api.HostHardware(store))
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

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("FleetCom listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
}
