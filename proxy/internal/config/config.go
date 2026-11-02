package config

import (
	"os"
	"strconv"
	"strings"
)

// Config represents runtime configuration for JellyProxy
type Config struct {
	Port         string
	UpstreamURL  string
	UpstreamHost string
	SharedSecret string
	CacheDir     string
	MaxStorageGB float64
	LogLevel     string
}

// Load reads configuration from environment variables with sensible defaults
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	upstream := os.Getenv("UPSTREAM_URL")
	if upstream == "" {
		upstream = "http://localhost:8096"
	}
	upstream = strings.TrimRight(upstream, "/")

	upstreamHost := os.Getenv("UPSTREAM_HOST")

	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/var/cache/jellyproxy"
	}

	maxStorageGB := 50.0
	if envMax := os.Getenv("MAX_STORAGE_GB"); envMax != "" {
		if parsed, err := strconv.ParseFloat(envMax, 64); err == nil && parsed > 0 {
			maxStorageGB = parsed
		}
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	return &Config{
		Port:         port,
		UpstreamURL:  upstream,
		UpstreamHost: upstreamHost,
		SharedSecret: os.Getenv("SHARED_SECRET"),
		CacheDir:     cacheDir,
		MaxStorageGB: maxStorageGB,
		LogLevel:     logLevel,
	}
}