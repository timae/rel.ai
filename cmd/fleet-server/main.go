// fleet-server is the team coordinator for ses capacity sharing: it receives
// per-seat usage heartbeats, serves the capacity board, and runs the task
// queue that `ses away` workers claim from.
//
// Config comes from env vars so the same binary works locally and on deploio:
//
//	FLEET_ADMIN_TOKEN     — required; bearer token for /v1/admin/* (user provisioning)
//	FLEET_DB              — SQLite path (default: ./data/fleet.db)
//	FLEET_ADDR            — listen address (default: :8081)
//	FLEET_SWEEP_INTERVAL  — lease/expiry sweeper interval (default: 1m)
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/timae/ses/internal/fleet"
)

func main() {
	logger := log.New(os.Stdout, "fleet-server ", log.LstdFlags|log.Lmsgprefix)

	adminToken := os.Getenv("FLEET_ADMIN_TOKEN")
	if adminToken == "" {
		logger.Fatal("FLEET_ADMIN_TOKEN is required")
	}
	dbPath := envOr("FLEET_DB", "./data/fleet.db")
	addr := envOr("FLEET_ADDR", ":8081")
	sweepInterval := envDuration("FLEET_SWEEP_INTERVAL", time.Minute, logger)

	store, err := fleet.OpenStore(dbPath)
	if err != nil {
		logger.Fatalf("init store: %v", err)
	}
	defer store.Close()

	srv, err := fleet.NewServer(fleet.Config{AdminToken: adminToken, Logger: logger}, store)
	if err != nil {
		logger.Fatalf("init server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go runSweeper(ctx, store, sweepInterval, logger)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s (db: %s)", addr, dbPath)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("serve: %v", err)
	}
}

func runSweeper(ctx context.Context, store *fleet.Store, interval time.Duration, logger *log.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			requeued, expired, failed, err := store.Sweep()
			if err != nil {
				logger.Printf("sweep: %v", err)
				continue
			}
			if requeued+expired+failed > 0 {
				logger.Printf("sweep: %d requeued, %d expired, %d failed (worker lost)", requeued, expired, failed)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration, logger *log.Logger) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Fatalf("invalid %s: %v", key, err)
	}
	return d
}
