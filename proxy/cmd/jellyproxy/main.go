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

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/auth"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/dashboard"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/handlers"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/playback"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/preseed"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/telemetry"
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

	// Live telemetry for the dashboard: every byte the proxy sends or pulls is
	// accounted here, so the UI can draw the client/server topology in real time.
	hub := telemetry.NewHub(12 * time.Second)
	names := telemetry.NewItemNames()
	proxyHandler.SetTelemetry(hub, names)

	// The shaper arbitrates the home upstream. Its settings come from the companion
	// plugin and the /proxy/ website, persisted in CacheDir/settings.json.
	abrManager := abr.NewBandwidthManager(config.DefaultABRConfig())
	_ = config.NewSettingsStore(cfg, abrManager, storageMgr)
	proxyHandler.SetABR(abrManager)
	abrAPI := &handlers.ABRAPI{Manager: abrManager}

	// The session tracker drives pause prefetching and seek borrowing, and feeds
	// the shaper and telemetry with playback state.
	sessions := playback.NewSessionTracker(cfg, storageMgr, proxyHandler.ActiveLiveUpstreamCount(), abrManager, names)
	proxyHandler.SetSniffer(sessions)

	handshakeMgr := handlers.NewHandshakeManager(cfg)
	handshakeMgr.SetManagers(abrManager, storageMgr)
	cacheAPI := handlers.NewCacheAPIHandler(cfg, storageMgr)
	preseedMgr := preseed.NewManager(storageMgr, cfg.UpstreamURL, cfg.UpstreamHost)
	preseedMgr.SetThrottle(abrManager.BackgroundWait)
	preseedAPI := handlers.NewPreseedAPI(cfg, preseedMgr)

	// Custom router multiplexer
	mux := http.NewServeMux()

	// Inter-service JellyProxy API endpoints
	mux.HandleFunc("/jellyproxy/api/status", handshakeMgr.HandleStatus)
	mux.HandleFunc("/jellyproxy/api/handshake", handshakeMgr.HandleHandshake)
	mux.HandleFunc("/jellyproxy/api/playback/report", auth.RequireSecret(cfg.SharedSecret, sessions.HandleReport))
	mux.HandleFunc("/jellyproxy/api/playback/sessions", auth.RequireSecret(cfg.SharedSecret, sessions.HandleSessions))
	mux.HandleFunc("/jellyproxy/api/storage", auth.RequireSecret(cfg.SharedSecret, cacheAPI.HandleStorage))
	mux.HandleFunc("/jellyproxy/api/config", auth.RequireSecret(cfg.SharedSecret, cacheAPI.HandleConfig))
	mux.HandleFunc("/jellyproxy/api/cache/purge", auth.RequireSecret(cfg.SharedSecret, cacheAPI.HandlePurge))
	mux.HandleFunc("/jellyproxy/api/cache/invalidate", auth.RequireSecret(cfg.SharedSecret, cacheAPI.HandleInvalidate))
	mux.HandleFunc("/jellyproxy/api/preseed/start", preseedAPI.Handler(preseedAPI.Start))
	mux.HandleFunc("/jellyproxy/api/preseed/jobs", preseedAPI.Handler(preseedAPI.Jobs))
	mux.HandleFunc("/jellyproxy/api/preseed/cancel", preseedAPI.Handler(preseedAPI.Cancel))
	mux.HandleFunc("/jellyproxy/api/abr/config", auth.RequireSecret(cfg.SharedSecret, abrAPI.Config))
	mux.HandleFunc("/jellyproxy/api/abr/status", auth.RequireSecret(cfg.SharedSecret, abrAPI.Status))
	mux.HandleFunc("/jellyproxy/api/abr/capabilities", auth.RequireSecret(cfg.SharedSecret, abrAPI.Capabilities))

	// Dashboard: probe the upstream so we never mount over a route Jellyfin already owns.
	dash := dashboard.New(cfg, dashboard.Options{
		ABR: abrManager, Storage: storageMgr, Hub: hub, Names: names,
		Sessions: sessions, Jobs: preseedMgr, Status: handshakeMgr,
	})
	if uiPath := dashboard.ResolveBasePath(cfg); uiPath != "" {
		dash.Mount(mux, uiPath)
	}

	// Catch-all: forward all standard Jellyfin traffic to upstream (with caching)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
