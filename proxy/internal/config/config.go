package config

import (
	"os"
	"strings"
)

// Config represents runtime configuration for JellyProxy
type Config struct {
	Port         string
	UpstreamURL  string
	SharedSecret string
	CacheDir     string
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

	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "/var/cache/jellyproxy"
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	return &Config{
		Port:         port,
		UpstreamURL:  upstream,
		SharedSecret: os.Getenv("SHARED_SECRET"),
		CacheDir:     cacheDir,
		LogLevel:     logLevel,
	}
}
