package handlers

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/Vigno04/jellyproxy/proxy/internal/config"
)

// ReverseProxyHandler forwards traffic transparently to upstream Jellyfin
type ReverseProxyHandler struct {
	proxy       *httputil.ReverseProxy
	upstreamURL *url.URL
}

// NewReverseProxyHandler creates an httputil reverse proxy instance
func NewReverseProxyHandler(cfg *config.Config) (*ReverseProxyHandler, error) {
	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Support internal / self-signed TLS certificates over VPN / Newt tunnel
	proxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		if cfg.UpstreamHost != "" {
			req.Host = cfg.UpstreamHost
		} else {
			req.Host = target.Host
		}

		// Set proxy headers
		if clientIP := req.Header.Get("X-Forwarded-For"); clientIP == "" {
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		}
		if proto := req.Header.Get("X-Forwarded-Proto"); proto == "" {
			if req.TLS != nil {
				req.Header.Set("X-Forwarded-Proto", "https")
			} else {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
		}

		// Ensure WebSocket upgrade headers survive proxying
		if isWebSocket(req) {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[JellyProxy] Upstream error for %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "JellyProxy: Upstream Jellyfin unreachable", http.StatusBadGateway)
	}

	return &ReverseProxyHandler{
		proxy:       proxy,
		upstreamURL: target,
	}, nil
}

// ServeHTTP delegates to httputil.ReverseProxy
func (h *ReverseProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.proxy.ServeHTTP(w, r)
}

func isWebSocket(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}
