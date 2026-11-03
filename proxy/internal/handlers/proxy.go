package handlers

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/telemetry"
)

// streamChunk is the unit in which upstream bytes are paced and accounted.
const streamChunk = 32 * 1024

// PlaylistSniffer is implemented by playback.SessionTracker to passively intercept HLS playlists and requests
type PlaylistSniffer interface {
	IngestPlaylist(playlistURL string, body string, headers http.Header)
	OnSegmentRequested(rawURL string, headers http.Header)
}

// ReverseProxyHandler forwards traffic transparently to upstream Jellyfin with edge caching for images and static assets
type ReverseProxyHandler struct {
	cfg                     *config.Config
	proxy                   *httputil.ReverseProxy
	upstreamURL             *url.URL
	httpClient              *http.Client
	storage                 *cache.StorageManager
	sniffer                 PlaylistSniffer
	activeLiveUpstreamCount int32
	coalescer               *cache.Coalescer
	abr                     *abr.BandwidthManager
	hub                     *telemetry.Hub
	names                   *telemetry.ItemNames
}

// SetSniffer sets the active playlist sniffer and segment tracker
func (h *ReverseProxyHandler) SetSniffer(s PlaylistSniffer) { h.sniffer = s }

// SetABR assigns the shared bandwidth shaper.
func (h *ReverseProxyHandler) SetABR(m *abr.BandwidthManager) { h.abr = m }

// SetTelemetry assigns the live traffic hub feeding the dashboard.
func (h *ReverseProxyHandler) SetTelemetry(hub *telemetry.Hub, names *telemetry.ItemNames) {
	h.hub, h.names = hub, names
}

// ActiveLiveUpstreamCount returns a pointer to the active live upstream downloads counter
func (h *ReverseProxyHandler) ActiveLiveUpstreamCount() *int32 { return &h.activeLiveUpstreamCount }

// NewReverseProxyHandler creates an httputil reverse proxy instance integrated with StorageManager
func NewReverseProxyHandler(cfg *config.Config, storage *cache.StorageManager) (*ReverseProxyHandler, error) {
	target, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		// The upstream is reached over a private tunnel with a self-signed certificate.
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

		if req.Header.Get("X-Forwarded-For") == "" {
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		}
		if req.Header.Get("X-Forwarded-Proto") == "" {
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
		storage:   storage,
		coalescer: cache.NewCoalescer(),
	}, nil
}

// ServeHTTP inspects requests for cache eligibility and either serves from cache, tees from upstream, or passes through
func (h *ReverseProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.abr != nil && r.Method == http.MethodPost && strings.HasSuffix(strings.ToLower(r.URL.Path), "/playbackinfo") {
		h.servePlaybackInfo(w, r)
		return
	}

	if r.Method == http.MethodGet && !isWebSocket(r) {
		if h.storage != nil {
			if category, itemID, cacheable := classifyCacheableRequest(r.URL.Path); cacheable {
				if category == cache.CategoryMedia {
					h.noteMediaRequest(r, itemID)
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

		if isDirectStream(r.URL.Path) {
			h.serveDirectStream(w, r)
			return
		}
	}

	h.proxy.ServeHTTP(w, r)
}

// noteMediaRequest keeps the client's pipe alive and tells the tracker playback is live.
func (h *ReverseProxyHandler) noteMediaRequest(r *http.Request, itemID string) {
	if h.sniffer != nil {
		h.sniffer.OnSegmentRequested(r.URL.RequestURI(), r.Header)
	}
	if h.abr != nil {
		clientID, _ := clientIdentity(r)
		h.abr.Touch(clientID, itemID)
	}
}

// serveDirectStream relays a whole-file stream (direct play) while pacing it to the
// shaper's share of the home upload and recording it for the dashboard.
func (h *ReverseProxyHandler) serveDirectStream(w http.ResponseWriter, r *http.Request) {
	itemID := extractItemIDFromMediaPath(r.URL.Path)
	clientID, clientName := clientIdentity(r)
	h.noteMediaRequest(r, itemID)

	var fetch *abr.Fetch
	if h.abr != nil {
		fetch = h.abr.BeginFetchMode(clientID, itemID, true)
		defer fetch.End()
	}
	atomic.AddInt32(&h.activeLiveUpstreamCount, 1)
	defer atomic.AddInt32(&h.activeLiveUpstreamCount, -1)

	ctx := r.Context()
	mw := &meteredWriter{ResponseWriter: w, onWrite: func(n int) {
		h.paceAndCount(ctx, fetch, itemID, n)
		h.hub.Sent(clientID, clientName, itemID, n, false)
	}}
	h.proxy.ServeHTTP(mw, r)
}

// paceAndCount charges n upstream bytes against the fetch's throughput share, blocking
// when the stream is running ahead of it, and records them as pulled from home.
func (h *ReverseProxyHandler) paceAndCount(ctx context.Context, fetch *abr.Fetch, itemID string, n int) {
	if fetch != nil {
		fetch.UpdateRate()
		_ = fetch.Wait(ctx, n)
		fetch.Account(n)
	}
	h.hub.Pulled(itemID, n)
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
		if strings.EqualFold(k, "Content-Length") {
			continue // body may have been rewritten
		}
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

func (h *ReverseProxyHandler) serveCachedOrFetch(w http.ResponseWriter, r *http.Request, category, itemID string) {
	keyHash := cache.HashKey(buildCacheKey(r, category))
	clientID, clientName := clientIdentity(r)

	// Check for cache HIT. Cache hits cost no home upload: this is what makes a viewer
	// who starts the same title later free (spec case 9).
	if file, meta, err := h.storage.Get(keyHash); err == nil {
		defer file.Close()
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

		var out http.ResponseWriter = w
		if category == cache.CategoryMedia {
			out = &meteredWriter{ResponseWriter: w, onWrite: func(n int) {
				h.hub.Sent(clientID, clientName, itemID, n, true)
			}}
		}
		// ServeContent supports HTTP Range requests (206), ETag and If-Modified-Since
		http.ServeContent(out, r, "media", meta.CreatedAt, file)
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

	outReq, err := h.buildUpstreamRequest(r)
	if err != nil {
		h.proxy.ServeHTTP(w, r)
		return
	}
	// Request uncompressed so cached payloads work for all clients
	outReq.Header.Del("Accept-Encoding")
	outReq.Header.Del("If-None-Match")
	outReq.Header.Del("If-Modified-Since")

	var fetch *abr.Fetch
	if category == cache.CategoryMedia {
		atomic.AddInt32(&h.activeLiveUpstreamCount, 1)
		defer atomic.AddInt32(&h.activeLiveUpstreamCount, -1)
		if h.abr != nil {
			fetch = h.abr.BeginFetchMode(clientID, itemID, false)
			defer fetch.End()
		}
	}

	resp, err := h.httpClient.Do(outReq)
	if err != nil {
		log.Printf("[JellyProxy] Upstream error for %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "JellyProxy: Upstream Jellyfin unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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

	// Stream to the client while teeing to disk. Only media chunks are paced and
	// accounted: they are the traffic that consumes the home upload.
	copyErr := h.teeStream(r.Context(), w, tmpFile, resp.Body, category == cache.CategoryMedia, fetch, itemID, clientID, clientName)
	closeErr := tmpFile.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return
	}

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

// teeStream copies src to both the client and the cache file in paced chunks.
func (h *ReverseProxyHandler) teeStream(ctx context.Context, client io.Writer, disk io.Writer, src io.Reader,
	media bool, fetch *abr.Fetch, itemID, clientID, clientName string) error {
	flusher, _ := client.(http.Flusher)
	buf := make([]byte, streamChunk)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if media {
				h.paceAndCount(ctx, fetch, itemID, n)
			}
			if _, err := disk.Write(buf[:n]); err != nil {
				return err
			}
			if _, err := client.Write(buf[:n]); err != nil {
				return err
			}
			if media {
				h.hub.Sent(clientID, clientName, itemID, n, false)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
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
