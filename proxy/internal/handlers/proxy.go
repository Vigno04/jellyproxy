package handlers

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Vigno04/jellyproxy/proxy/internal/cache"
	"github.com/Vigno04/jellyproxy/proxy/internal/config"
	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
)

var staticCacheExtensions = []string{
	".js", ".css", ".woff", ".woff2", ".svg", ".png", ".jpg", ".ico",
}

var mediaSegmentExtensions = []string{
	".m4s", ".ts", ".mp4", ".m4a", ".aac", ".vtt", ".bif",
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

// PlaylistSniffer is implemented by playback.SessionTracker to passively intercept HLS playlists and requests
type PlaylistSniffer interface {
	IngestPlaylist(playlistURL string, body string, headers http.Header)
	OnSegmentRequested(rawURL string, headers http.Header)
}

// ReverseProxyHandler forwards traffic transparently to upstream Jellyfin with edge caching for images and static assets
type ReverseProxyHandler struct {
	cfg                    *config.Config
	proxy                  *httputil.ReverseProxy
	upstreamURL            *url.URL
	httpClient             *http.Client
	storage                *cache.StorageManager
	sniffer                PlaylistSniffer
	activeLiveUpstreamCount int32
	coalescer              *cache.Coalescer
	abr                    *abr.BandwidthManager
}

// SetSniffer sets the active playlist sniffer and segment tracker
func (h *ReverseProxyHandler) SetSniffer(s PlaylistSniffer) {
	h.sniffer = s
}

// ActiveLiveUpstreamCount returns a pointer to the active live upstream downloads counter
func (h *ReverseProxyHandler) ActiveLiveUpstreamCount() *int32 {
	return &h.activeLiveUpstreamCount
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
		coalescer: cache.NewCoalescer(),
		abr: abr.NewBandwidthManager(abr.Config{Enabled: true, UploadSpeedBps: 16_000_000}),
	}, nil
}

// ServeHTTP inspects requests for cache eligibility and either serves from cache, tees from upstream, or passes through
func (h *ReverseProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.abr != nil && r.Method == http.MethodPost && strings.HasSuffix(strings.ToLower(r.URL.Path), "/playbackinfo") {
		h.servePlaybackInfo(w, r)
		return
	}
	if h.storage != nil && r.Method == http.MethodGet && !isWebSocket(reqOrNil(r)) {
		if category, itemID, cacheable := classifyCacheableRequest(r.URL.Path); cacheable {
			if category == cache.CategoryMedia && h.sniffer != nil {
				h.sniffer.OnSegmentRequested(r.URL.RequestURI(), r.Header)
			}
			h.serveCachedOrFetch(w, r, category, itemID)
			return
		}

		// Intercept HLS playlists (.m3u8) to sniff upcoming media segments for prefetch
		if strings.HasSuffix(strings.ToLower(r.URL.Path), ".m3u8") && h.sniffer != nil {
			h.capturePlaylist(w, r)
			return
		}
	}

	h.proxy.ServeHTTP(w, r)
}

func (h *ReverseProxyHandler) capturePlaylist(w http.ResponseWriter, r *http.Request) {
	outReq, err := h.buildUpstreamRequest(r)
	if err != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}

	resp, err := h.httpClient.Do(outReq)
	if err != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read upstream playlist", http.StatusBadGateway)
		return
	}

	if resp.StatusCode == http.StatusOK && strings.HasSuffix(strings.ToLower(r.URL.Path), "master.m3u8") && h.abr != nil {
		bodyBytes = h.filterMasterPlaylist(r, bodyBytes)
	}

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bodyBytes)

	if resp.StatusCode == http.StatusOK && h.sniffer != nil {
		h.sniffer.IngestPlaylist(r.URL.RequestURI(), string(bodyBytes), r.Header)
	}
}

func (h *ReverseProxyHandler) filterMasterPlaylist(r *http.Request, body []byte) []byte {
	client := r.URL.Query().Get("DeviceId")
	if client == "" { client = r.URL.Query().Get("PlaySessionId") }
	if client == "" { client = r.RemoteAddr }
	status := h.abr.GetStatus()
	var clientAlloc int64
	for _, p := range status.Pipes {
		if p.AllocatedBps > 0 { clientAlloc = p.AllocatedBps; break }
	}
	if clientAlloc <= 0 { return body }

	lines := strings.Split(string(body), "\n")
	type variant struct {
		inf  string
		url  string
		bw   int64
	}
	var variants []variant
	var otherLines []string
	var minBw int64 = 1<<62 - 1

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF:") {
			var bw int64
			for _, part := range strings.Split(strings.TrimPrefix(trimmed, "#EXT-X-STREAM-INF:"), ",") {
				if strings.HasPrefix(part, "BANDWIDTH=") {
					fmt.Sscanf(strings.TrimPrefix(part, "BANDWIDTH="), "%d", &bw)
				}
			}
			urlLine := ""
			if i+1 < len(lines) {
				urlLine = lines[i+1]
				i++
			}
			if bw > 0 && bw < minBw { minBw = bw }
			variants = append(variants, variant{inf: line, url: urlLine, bw: bw})
		} else {
			otherLines = append(otherLines, line)
		}
	}

	if len(variants) == 0 { return body }

	var filtered []string
	for _, v := range variants {
		// Retain variants <= clientAlloc or the absolute lowest fallback variant
		if v.bw <= clientAlloc || v.bw == minBw {
			filtered = append(filtered, v.inf, v.url)
		}
	}

	// Reconstruct playlist
	var out []string
	for _, l := range otherLines {
		out = append(out, l)
		if strings.HasPrefix(strings.TrimSpace(l), "#EXTM3U") {
			out = append(out, filtered...)
		}
	}
	return []byte(strings.Join(out, "\n"))
}

func (h *ReverseProxyHandler) servePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	item := "unknown"
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ { if strings.EqualFold(parts[i], "Items") { item=parts[i+1]; break } }
	client := r.URL.Query().Get("DeviceId"); if client=="" {client=r.URL.Query().Get("PlaySessionId")}; if client=="" {client=r.RemoteAddr}
	requested:=int64(0); if v:=r.URL.Query().Get("maxStreamingBitrate");v!="" { for _,c:=range v {if c>='0'&&c<='9' {requested=requested*10+int64(c-'0')}} }; if requested==0 {requested=100_000_000}
	alloc:=h.abr.Allocate(client,item,requested); q:=r.URL.Query();q.Set("maxStreamingBitrate",fmt.Sprint(alloc));r.URL.RawQuery=q.Encode()
	if r.Body!=nil {b,_:=io.ReadAll(r.Body); var v map[string]any; if json.Unmarshal(b,&v)==nil { for _,k:=range []string{"MaxStreamingBitrate","MaxStaticBitrate"}{v[k]=alloc}; if d,ok:=v["DeviceProfile"].(map[string]any);ok {d["MaxStreamingBitrate"]=alloc}; b,_=json.Marshal(v);r.Body=io.NopCloser(bytes.NewReader(b));r.ContentLength=int64(len(b)) } }
	h.proxy.ServeHTTP(w,r)
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

	// 3. Media Segments (HLS fMP4 / TS / init.mp4 / subtitles / trickplay)
	// Never cache dynamic playlists (.m3u8, .mpd)
	if !strings.HasSuffix(lowerPath, ".m3u8") && !strings.HasSuffix(lowerPath, ".mpd") {
		if strings.Contains(lowerPath, "/videos/") || strings.Contains(lowerPath, "/audio/") ||
			strings.Contains(lowerPath, "/hls/") || strings.Contains(lowerPath, "/hls1/") {
			for _, ext := range mediaSegmentExtensions {
				if strings.HasSuffix(lowerPath, ext) {
					return cache.CategoryMedia, extractItemIDFromMediaPath(rawPath), true
				}
			}
		}
	}

	return "", "", false
}

// extractItemIDFromMediaPath parses the Jellyfin item ID from video/audio stream paths
func extractItemIDFromMediaPath(rawPath string) string {
	segments := strings.Split(strings.Trim(rawPath, "/"), "/")
	for i := 0; i < len(segments)-1; i++ {
		lower := strings.ToLower(segments[i])
		if lower == "videos" || lower == "audio" {
			candidate := segments[i+1]
			if candidate != "" && !strings.Contains(candidate, ".") {
				return candidate
			}
		}
	}
	return ""
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

// buildCacheKey delegates to the canonical cache key implementation.
func buildCacheKey(r *http.Request, category string) string {
	return cache.BuildKey(category, r.URL.RequestURI())
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
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-JellyProxy-Cache", "HIT")

		// Serve using http.ServeContent to support HTTP Range requests (206 Partial Content), ETag, and If-Modified-Since
		http.ServeContent(w, r, "media", meta.CreatedAt, file)
		return
	}

	// Coalesce concurrent misses so only one request downloads the object.
	call, owner := h.coalescer.Start(keyHash)
	if !owner {
		select {
		case <-call.Done():
			h.serveCachedOrFetch(w, r, category, itemID)
		case <-r.Context().Done():
			http.Error(w, "request cancelled", http.StatusRequestTimeout)
		}
		return
	}
	defer h.coalescer.Finish(keyHash, call)

	// Cache MISS: if client sent a Range header on non-segment request, proxy directly to prevent caching partial fragments
	if r.Header.Get("Range") != "" && category != cache.CategoryMedia {
		h.proxy.ServeHTTP(w, r)
		return
	}

	// Fetch from upstream
	outReq, err := h.buildUpstreamRequest(r)
	if err != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}
	// Request uncompressed from upstream or preserve straightforward encoding so cached payloads work for all clients
	outReq.Header.Del("Accept-Encoding")
	outReq.Header.Del("If-None-Match")
	outReq.Header.Del("If-Modified-Since")

	if category == cache.CategoryMedia {
		atomic.AddInt32(&h.activeLiveUpstreamCount, 1)
		defer atomic.AddInt32(&h.activeLiveUpstreamCount, -1)
	}

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