package main

// Route registration lives in router.go (FLEET-80 extracted it so the
// drift-protection test can build a router without duplicating wiring).
// SSE event mapping (FLEET-71) is also documented in router.go.

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/markus-barta/fleetcom/internal/auth"
	"github.com/markus-barta/fleetcom/internal/db"
	"github.com/markus-barta/fleetcom/internal/openclaw"
	"github.com/markus-barta/fleetcom/internal/sse"
	"github.com/markus-barta/fleetcom/internal/version"
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

	// OpenClaw WS manager — one WebSocket connection per paired gateway
	// for pairing events + auto-approval. Skips gateways without on-disk
	// keypairs (FLEET-52 pre-seed) so boot is quiet until secrets exist.
	ocKeyDir := os.Getenv("FLEETCOM_OPENCLAW_KEY_DIR")
	if ocKeyDir == "" {
		// Default scans both agenix (nixcfg-managed pairings) and the
		// in-container data dir (wizard-generated pairings, FLEET-61).
		ocKeyDir = "/run/agenix:/app/data/openclaw-keys"
	}
	ocMgr := openclaw.NewManager(store, hub, ocKeyDir, version.Version)
	ocCtx, ocCancel := context.WithCancel(context.Background())
	defer ocCancel()
	ocMgr.Start(ocCtx)

	// Purge samples older than 400 days (covers the 1Y history scale).
	const sampleRetention = 400 * 24 * time.Hour
	// Keep container events for 24 hours (only needed for crash loop detection).
	const eventRetention = 24 * time.Hour
	// Keep host hardware metrics for 24h (sparklines are a rolling 1h window;
	// 24h gives headroom and is still tiny — ~1440 rows/host).
	const hostMetricsRetention = 24 * time.Hour
	// Agent observability: match status samples (400d rolling).
	const agentObsRetention = sampleRetention
	// FLEET-108: activity log — keep most rows 7 days; CREATE/DELETE/GRANT/
	// REVOKE rows kept 30 days because operators look back at those when
	// investigating "who changed what last quarter".
	const activityShortRetention = 7 * 24 * time.Hour
	const activityLongRetention = 30 * 24 * time.Hour
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
	if n, err := store.PruneOldAgentObs(agentObsRetention); err != nil {
		log.Printf("initial agent_obs purge failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old agent rows", n)
	}
	if n, err := store.PruneOldActivity(activityShortRetention, activityLongRetention); err != nil {
		log.Printf("initial activity prune failed: %v", err)
	} else if n > 0 {
		log.Printf("purged %d old activity rows", n)
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
			if n, err := store.PruneOldAgentObs(agentObsRetention); err != nil {
				log.Printf("agent_obs purge failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old agent rows", n)
			}
			if n, err := store.PruneOldActivity(activityShortRetention, activityLongRetention); err != nil {
				log.Printf("activity prune failed: %v", err)
			} else if n > 0 {
				log.Printf("purged %d old activity rows", n)
			}
			store.CleanExpiredSessions()
			store.CleanExpiredTOTPPending()
		}
	}()

	// FLEET-60: command expiry. A command in 'executing' with picked_at
	// older than 5m means bosun either crashed mid-execution or its
	// reply POST got lost. Mark them failed so the UI doesn't show them
	// as forever-in-progress. Runs separately from the 6h cleanup loop
	// because 5m latency matters here.
	go func() {
		const stuckMaxAge = 5 * time.Minute
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for range t.C {
			if n, err := store.ExpireStuckCommands(stuckMaxAge); err != nil {
				log.Printf("command expiry failed: %v", err)
			} else if n > 0 {
				log.Printf("expired %d stuck host commands", n)
			}
		}
	}()

	r := newRouter(&routerDeps{
		store:         store,
		hub:           hub,
		auth:          a,
		resetHandlers: resetHandlers,
		ocMgr:         ocMgr,
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
		log.Printf("FleetCom listening on :%s (version %s)", port, version.Version)
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
