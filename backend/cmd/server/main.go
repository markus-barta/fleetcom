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

	hub := sse.NewHub()
	a := auth.New(store)

	// Purge samples older than 400 days (covers the 1Y history scale).
	const sampleRetention = 400 * 24 * time.Hour
	if n, err := store.PurgeOldSamples(sampleRetention); err != nil {
		log.Printf("initial sample purge failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old status samples", n)
	}
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if n, err := store.PurgeOldSamples(sampleRetention); err != nil {
				log.Printf("sample purge failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old status samples", n)
			}
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
	r.Get("/LICENSE", api.License)
	r.Post("/api/heartbeat", api.Heartbeat(store, hub))
	r.Get("/login", api.LoginPage)
	r.Post("/login", a.HandleLogin)
	r.Get("/logout", a.HandleLogout)

	// Static files (public — CSS, JS, images)
	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))

	// Share links (read-only, token-authenticated)
	r.Get("/s/{token}", api.SharedDashboard(store))
	r.Get("/s/{token}/events", api.SharedEvents(store, hub))

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(a.RequireSession)
		r.Get("/", api.Dashboard)
		r.Get("/api/events", api.Events(store, hub))
		r.Get("/api/hosts", api.ListHosts(store))
		r.Get("/api/tokens", api.ListTokens(store))
		r.Post("/api/hosts", api.AddHost(store))
		r.Delete("/api/hosts", api.DeleteHost(store))
		r.Get("/api/shares", api.ListShareLinks(store))
		r.Post("/api/shares", api.CreateShareLink(store))
		r.Delete("/api/shares", api.DeleteShareLink(store))
		r.Get("/api/history", api.History(store))
		r.Put("/api/settings", api.UpdateSettings(store, hub))
		r.Get("/api/ignored", api.ListIgnored(store))
		r.Post("/api/ignore", api.AddIgnore(store, hub))
		r.Delete("/api/ignore", api.RemoveIgnore(store, hub))
		r.Get("/api/host-configs", api.GetHostConfigs(store))
		r.Put("/api/host-config", api.UpdateHostConfig(store, hub))
		r.Get("/api/image-presets", api.ListImagePresets(store))
		r.Post("/api/image-presets", api.UploadImagePreset(store))
		r.Delete("/api/image-presets/{id}", api.DeleteImagePreset(store))
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
