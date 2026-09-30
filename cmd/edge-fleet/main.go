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

	server := &http.Server{Addr: addr, Handler: fleet.NewHandler(fleet.NewStore())}
	log.Printf("edge fleet hub listening on http://%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
