package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/handlers"
)

func main() {
	cfg := config.Load()

	log.Printf("Starting JellyProxy v0.1.0...")
	log.Printf("Listening on port: :%s", cfg.Port)
	log.Printf("Target upstream Jellyfin: %s", cfg.UpstreamURL)
	if cfg.UpstreamHost != "" {
		log.Printf("Upstream Host header override: %s", cfg.UpstreamHost)
	}
	if cfg.SharedSecret != "" {
		log.Printf("Shared Secret authentication: ENABLED")
	} else {
		log.Printf("Shared Secret authentication: DISABLED (open pairing mode)")
	}
	log.Printf("Cache directory: %s (Max storage: %.2f GB)", cfg.CacheDir, cfg.MaxStorageGB)

	storageMgr, err := cache.NewStorageManager(cfg.CacheDir, cfg.MaxStorageGB)
	if err != nil {
		log.Fatalf("Failed to initialize cache storage manager: %v", err)
	}

	proxyHandler, err := handlers.NewReverseProxyHandler(cfg, storageMgr)
	if err != nil {
		log.Fatalf("Failed to initialize reverse proxy: %v", err)
	}

	handshakeMgr := handlers.NewHandshakeManager(cfg)
	cacheAPI := handlers.NewCacheAPIHandler(cfg, storageMgr)

	// Custom router multiplexer
	mux := http.NewServeMux()

	// Inter-service JellyProxy API endpoints
	mux.HandleFunc("/jellyproxy/api/status", handshakeMgr.HandleStatus)
	mux.HandleFunc("/jellyproxy/api/handshake", handshakeMgr.HandleHandshake)
	mux.HandleFunc("/jellyproxy/api/storage", cacheAPI.HandleStorage)
	mux.HandleFunc("/jellyproxy/api/config", cacheAPI.HandleConfig)
	mux.HandleFunc("/jellyproxy/api/cache/purge", cacheAPI.HandlePurge)
	mux.HandleFunc("/jellyproxy/api/cache/invalidate", cacheAPI.HandleInvalidate)

	// Catch-all: forward all standard Jellyfin traffic to upstream (with caching)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Keep internal API isolated
		if strings.HasPrefix(r.URL.Path, "/jellyproxy/api/") {
			http.NotFound(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Minute, // High timeouts for streaming
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	// Graceful shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stopChan
	log.Println("Shutting down JellyProxy gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	log.Println("JellyProxy stopped.")
}