package handlers

import (
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
)

var staticCacheExtensions = []string{
	".js", ".css", ".woff", ".woff2", ".svg", ".png", ".jpg", ".ico",
}

var cachedResponseHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"ETag",
	"Last-Modified",
	"Expires",
	"Vary",
	"Content-Encoding",
}

// ReverseProxyHandler forwards traffic transparently to upstream Jellyfin with edge caching for images and static assets
type ReverseProxyHandler struct {
	cfg         *config.Config
	proxy       *httputil.ReverseProxy
	upstreamURL *url.URL
	httpClient  *http.Client
	storage     *cache.StorageManager
}

// NewReverseProxyHandler creates an httputil reverse proxy instance integrated with StorageManager
func NewReverseProxyHandler(cfg *config.Config, storage *cache.StorageManager) (*ReverseProxyHandler, error) {
	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

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
		cfg:         cfg,
		proxy:       proxy,
		upstreamURL: target,
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		storage: storage,
	}, nil
}

// ServeHTTP inspects requests for cache eligibility and either serves from cache, tees from upstream, or passes through
func (h *ReverseProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.storage != nil && r.Method == http.MethodGet && !isWebSocket(reqOrNil(r)) {
		if category, itemID, cacheable := classifyCacheableRequest(r.URL.Path); cacheable {
			h.serveCachedOrFetch(w, r, category, itemID)
			return
		}
	}

	h.proxy.ServeHTTP(w, r)
}

func reqOrNil(r *http.Request) *http.Request {
	return r
}

// classifyCacheableRequest determines if a request path is eligible for edge caching,
// returning its category ("images" or "static"), extracted Jellyfin item ID (if any), and eligibility boolean.
func classifyCacheableRequest(rawPath string) (category string, itemID string, ok bool) {
	lowerPath := strings.ToLower(rawPath)

	// 1. Static web assets under /web/
	if idx := strings.Index(lowerPath, "/web/"); idx != -1 {
		subPath := lowerPath[idx:] // starts with "/web/"
		if subPath == "/web/index.html" || subPath == "/web/config.json" ||
			strings.HasSuffix(subPath, "/web/index.html") || strings.HasSuffix(subPath, "/web/config.json") {
			return "", "", false
		}
		for _, ext := range staticCacheExtensions {
			if strings.HasSuffix(subPath, ext) {
				return cache.CategoryStatic, "", true
			}
		}
		return "", "", false
	}

	// 2. Jellyfin Images: paths containing /Images/ or matching /Items/{id}/Images/{type}
	if strings.Contains(lowerPath, "/images/") {
		return cache.CategoryImages, extractItemIDFromImagePath(rawPath), true
	}

	return "", "", false
}

// extractItemIDFromImagePath parses the Jellyfin item ID from paths like:
// - /Items/{id}/Images/{type}
// - /Users/{userId}/Items/{id}/Images/{type}
// - /emby/Items/{id}/Images/{type}
// - /Persons/{id}/Images/{type}
// - /Artists/{id}/Images/{type}
// - /Genres/{id}/Images/{type}
// - /Studios/{id}/Images/{type}
// - /Users/{id}/Images/{type}
func extractItemIDFromImagePath(rawPath string) string {
	segments := strings.Split(strings.Trim(rawPath, "/"), "/")
	// Look specifically for "Items/{id}/Images" first
	for i := 0; i < len(segments)-2; i++ {
		if strings.EqualFold(segments[i], "Items") && strings.EqualFold(segments[i+2], "Images") {
			return segments[i+1]
		}
	}
	// Fallback: any "{Entity}/{id}/Images"
	for i := 1; i < len(segments); i++ {
		if strings.EqualFold(segments[i], "Images") && i >= 1 {
			candidate := segments[i-1]
			if !strings.EqualFold(candidate, "emby") && !strings.EqualFold(candidate, "jellyfin") {
				return candidate
			}
		}
	}
	return ""
}

// buildCacheKey creates a normalized cache key from request path, query parameters, and encoding
func buildCacheKey(r *http.Request, category string) string {
	// Include Accept-Encoding distinction only for static assets where upstream might compress
	var b strings.Builder
	b.WriteString(category)
	b.WriteString(":")
	b.WriteString(r.URL.Path)

	if r.URL.RawQuery != "" {
		// Strip volatile auth tokens from cache key so identical images across sessions hit the same cache file
		q := r.URL.Query()
		q.Del("api_key")
		q.Del("ApiKey")
		encoded := q.Encode()
		if encoded != "" {
			b.WriteString("?")
			b.WriteString(encoded)
		}
	}
	return b.String()
}

func (h *ReverseProxyHandler) serveCachedOrFetch(w http.ResponseWriter, r *http.Request, category, itemID string) {
	rawKey := buildCacheKey(r, category)
	keyHash := cache.HashKey(rawKey)

	// Check for cache HIT
	if file, meta, err := h.storage.Get(keyHash); err == nil {
		defer file.Close()

		// Restore cached headers
		for k, vals := range meta.Headers {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		if meta.ContentType != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", meta.ContentType)
		}
		if meta.ETag != "" && w.Header().Get("ETag") == "" {
			w.Header().Set("ETag", meta.ETag)
		}
		if meta.LastModified != "" && w.Header().Get("Last-Modified") == "" {
			w.Header().Set("Last-Modified", meta.LastModified)
		}
		w.Header().Set("X-JellyProxy-Cache", "HIT")

		// Handle conditional requests (If-None-Match)
		if clientETag := r.Header.Get("If-None-Match"); clientETag != "" && meta.ETag != "" {
			if clientETag == meta.ETag || clientETag == "*" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		if meta.Size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, file)
		return
	}

	// Cache MISS: fetch from upstream
	outReq, err := h.buildUpstreamRequest(r)
	if err != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}
	// Request uncompressed from upstream or preserve straightforward encoding so cached payloads work for all clients
	outReq.Header.Del("Accept-Encoding")
	outReq.Header.Del("If-None-Match")
	outReq.Header.Del("If-Modified-Since")

	resp, err := h.httpClient.Do(outReq)
	if err != nil {
		log.Printf("[JellyProxy] Upstream error for %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "JellyProxy: Upstream Jellyfin unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy upstream response headers to client
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-JellyProxy-Cache", "MISS")

	// Only cache 200 OK responses
	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	tmpFile, err := h.storage.CreateTempFile()
	if err != nil {
		log.Printf("[JellyProxy] Warning: failed to create temp cache file: %v", err)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmpPath := tmpFile.Name()

	w.WriteHeader(http.StatusOK)

	// Stream response to client while teeing to temporary file on disk
	teeReader := io.TeeReader(resp.Body, tmpFile)
	_, copyErr := io.Copy(w, teeReader)
	closeErr := tmpFile.Close()

	if copyErr != nil || closeErr != nil {
		_ = h.storage.Delete(keyHash)
		return
	}

	// Capture relevant headers for metadata
	savedHeaders := make(map[string][]string)
	for _, hdr := range cachedResponseHeaders {
		if vals := resp.Header.Values(hdr); len(vals) > 0 {
			savedHeaders[hdr] = append([]string(nil), vals...)
		}
	}

	meta := cache.CacheMetadata{
		Key:          keyHash,
		URL:          r.URL.RequestURI(),
		Category:     category,
		ItemID:       itemID,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Headers:      savedHeaders,
		CreatedAt:    time.Now().UTC(),
	}

	if err := h.storage.CommitTempFile(tmpPath, meta); err != nil {
		log.Printf("[JellyProxy] Warning: failed to commit cache file for %s: %v", r.URL.Path, err)
	}
}

func (h *ReverseProxyHandler) buildUpstreamRequest(r *http.Request) (*http.Request, error) {
	targetQuery := h.upstreamURL.RawQuery
	outURL := *h.upstreamURL
	outURL.Path = singleJoiningSlash(h.upstreamURL.Path, r.URL.Path)
	if targetQuery == "" || r.URL.RawQuery == "" {
		outURL.RawQuery = targetQuery + r.URL.RawQuery
	} else {
		outURL.RawQuery = targetQuery + "&" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), nil)
	if err != nil {
		return nil, err
	}

	for k, vals := range r.Header {
		for _, v := range vals {
			outReq.Header.Add(k, v)
		}
	}

	if h.cfg.UpstreamHost != "" {
		outReq.Host = h.cfg.UpstreamHost
	} else {
		outReq.Host = h.upstreamURL.Host
	}

	if clientIP := outReq.Header.Get("X-Forwarded-For"); clientIP == "" {
		outReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	if proto := outReq.Header.Get("X-Forwarded-Proto"); proto == "" {
		if r.TLS != nil {
			outReq.Header.Set("X-Forwarded-Proto", "https")
		} else {
			outReq.Header.Set("X-Forwarded-Proto", "http")
		}
	}

	return outReq, nil
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func isWebSocket(req *http.Request) bool {
	if req == nil {
		return false
	}
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}