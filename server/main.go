package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	cfg := loadConfig()
	srv := NewServer(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/v1/session/", srv.routeSession)

	httpServer := &http.Server{
		Addr:              ":",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	httpServer.Addr = addr

	// Start background cleaner.
	go srv.cleanerLoop()

	log.Printf("uplink WebSocket server listening on %s", addr)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	waitForShutdown(httpServer)

	// Give in-flight connections a moment to drain.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	log.Println("server stopped")
}
