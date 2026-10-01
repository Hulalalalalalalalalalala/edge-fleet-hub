package main

import (
	"log"
	"net/http"
	"os"

	"github.com/Hulalalalalalalalalalala/edge-fleet-hub/internal/fleet"
)

func main() {
	addr := os.Getenv("EDGE_FLEET_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	var store *fleet.Store
	if dataDir := os.Getenv("EDGE_FLEET_DATA_DIR"); dataDir != "" {
		var err error
		store, err = fleet.NewPersistentStore(dataDir)
		if err != nil {
			log.Fatalf("persistence setup failed: %v", err)
		}
		defer store.Close()
		log.Printf("edge fleet hub using data directory %s", dataDir)
	} else {
		store = fleet.NewStore()
	}

	server := &http.Server{Addr: addr, Handler: fleet.NewHandler(store)}
	log.Printf("edge fleet hub listening on http://%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
