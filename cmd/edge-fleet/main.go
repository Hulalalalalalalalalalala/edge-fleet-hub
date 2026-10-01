package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Hulalalalalalalalalalala/edge-fleet-hub/internal/fleet"
)

func main() {
	addr := os.Getenv("EDGE_FLEET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	store := fleet.NewStore()
	persistent := false
	if dataDir := strings.TrimSpace(os.Getenv("EDGE_FLEET_DATA_DIR")); dataDir != "" {
		// Recovery (and inter-process locking) completes before the server
		// accepts any business request. Failure here is fatal; the service
		// never silently falls back to in-memory mode.
		recovered, err := fleet.NewPersistentStore(dataDir)
		if err != nil {
			log.Fatalf("edge fleet hub: opening data directory %s failed: %v", dataDir, err)
		}
		store = recovered
		persistent = true
		defer func() {
			if err := store.Close(); err != nil {
				log.Printf("edge fleet hub: closing data directory failed: %v", err)
			}
		}()
	}

	server := &http.Server{Addr: addr, Handler: fleet.NewHandler(store)}
	if persistent {
		log.Printf("edge fleet hub listening on http://%s (persistent mode, data dir %s)", addr, os.Getenv("EDGE_FLEET_DATA_DIR"))
	} else {
		log.Printf("edge fleet hub listening on http://%s (in-memory mode)", addr)
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		// Let in-flight writes finish (they reach their durable commit point
		// before responding); requests still cut off remain unacknowledged and
		// may be retried after restart.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("edge fleet hub: shutdown error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
