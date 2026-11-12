package handlers

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
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
	lowerReqPath := strings.ToLower(r.URL.Path)
	if h.abr != nil && r.Method == http.MethodPost && strings.HasSuffix(lowerReqPath, "/playbackinfo") {
		h.servePlaybackInfo(w, r)
		return
	}
	if h.abr != nil && ((r.Method == http.MethodDelete && strings.HasSuffix(lowerReqPath, "/videos/activeencodings")) ||
		(r.Method == http.MethodPost && strings.HasSuffix(lowerReqPath, "/sessions/playing/stopped"))) {
		clientID, _ := clientIdentity(r)
		if clientID == "" {
			clientID = r.URL.Query().Get("deviceId")
		}
		if clientID != "" {
			if activePSID := h.abr.ActivePlaySessionID(clientID); activePSID != "" {
				h.stopUpstreamEncoding(r, r.URL.Query(), activePSID)
			}
			h.abr.Release(clientID)
		}
	}

	if r.Method == http.MethodGet && !isWebSocket(r) {
		if strings.HasSuffix(strings.ToLower(r.URL.Path), "/bitratetest") {
			h.serveBitrateTest(w, r)
			return
		}
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

	// Cache MISS: if client sent a Range header on non-media request, proxy directly to prevent caching partial fragments.
	// For media requests with a Range header, strip Range toward upstream so we fetch and cache the full object,
	// then serve the requested byte range from the cached file via http.ServeContent.
	isMediaRange := r.Header.Get("Range") != "" && category == cache.CategoryMedia
	if r.Header.Get("Range") != "" && !isMediaRange {
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
	sharedTry := false
	restartedEncoding := false
	var activeQuery url.Values
	lowerPath := strings.ToLower(r.URL.Path)
	isInitSeg := strings.HasSuffix(lowerPath, "/-1.mp4") || strings.HasSuffix(lowerPath, "/init.mp4")
	if category == cache.CategoryMedia {
		atomic.AddInt32(&h.activeLiveUpstreamCount, 1)
		defer atomic.AddInt32(&h.activeLiveUpstreamCount, -1)
		if h.abr != nil {
			fetch = h.abr.BeginFetchMode(clientID, itemID, false)
			defer fetch.End()
			if !isMediaRange {
				rq := r.URL.Query()
				if mergedQuery, ownerHdrs, ok := fetch.PrepareUpstream(clientID, rq); ok {
					h.applyUpstreamQuery(outReq, mergedQuery)
					for k, v := range ownerHdrs {
						outReq.Header.Set(k, v)
					}
					sharedTry = true
				} else if !isInitSeg {
					urlVideoBps, _ := strconv.ParseInt(rq.Get("VideoBitrate"), 10, 64)
					urlAudioBps, _ := strconv.ParseInt(rq.Get("AudioBitrate"), 10, 64)
					urlPlaySessionID := rq.Get("PlaySessionId")
					if urlVideoBps > 0 {
						targetVideoBps, activePSID, oldPSID, restart := fetch.CheckBitrateSwitch(urlVideoBps, urlAudioBps, urlPlaySessionID)
						if restart {
							log.Printf("[JellyProxy] On-the-fly bitrate switch for item=%s client=%s (%s): VideoBitrate %d -> %d PlaySessionId %s -> %s (%s)",
								itemID, clientID, clientName, urlVideoBps, targetVideoBps, oldPSID, activePSID, r.URL.Path)
							h.stopUpstreamEncoding(r, rq, oldPSID)
							restartedEncoding = true
						}
						changed := false
						if targetVideoBps > 0 && targetVideoBps != urlVideoBps {
							rq.Set("VideoBitrate", strconv.FormatInt(targetVideoBps, 10))
							changed = true
						}
						if activePSID != "" && activePSID != urlPlaySessionID {
							rq.Set("PlaySessionId", activePSID)
							changed = true
						}
						if changed {
							h.applyUpstreamQuery(outReq, rq.Encode())
							activeQuery = rq
						}
					}
				}
			}
		}
	}
	if isMediaRange {
		outReq.Header.Del("Range")
		outReq.Header.Del("If-Range")
	}

	isSub := strings.Contains(lowerPath, "/subtitles/")
	if isSub {
		subCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		outReq = outReq.WithContext(subCtx)
	}

	resp, err := h.httpClient.Do(outReq)
	if err == nil && resp.StatusCode != http.StatusOK && sharedTry {
		_ = resp.Body.Close()
		if retryReq, rerr := h.buildUpstreamRequest(r); rerr == nil {
			retryReq.Header.Del("Accept-Encoding")
			retryReq.Header.Del("If-None-Match")
			retryReq.Header.Del("If-Modified-Since")
			if isMediaRange {
				retryReq.Header.Del("Range")
				retryReq.Header.Del("If-Range")
			}
			sharedTry = false
			resp, err = h.httpClient.Do(retryReq)
			if err == nil && resp.StatusCode == http.StatusOK && fetch != nil && !isMediaRange {
				fetch.RecordUpstream(clientID, r.URL.Query(), r.Header, true)
			}
		}
	}
	if err != nil {
		log.Printf("[JellyProxy] Upstream error for %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "JellyProxy: Upstream Jellyfin unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Only cache 200 OK responses
	if resp.StatusCode != http.StatusOK {
		for k, vals := range resp.Header {
			if restartedEncoding && strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-JellyProxy-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if fetch != nil && !sharedTry && !isMediaRange {
		recQ := r.URL.Query()
		if activeQuery != nil {
			recQ = activeQuery
		}
		fetch.RecordUpstream(clientID, recQ, r.Header, false)
	}

	var bodyReader io.Reader = resp.Body
	if restartedEncoding && !isInitSeg && (strings.HasSuffix(lowerPath, ".mp4") || strings.HasSuffix(lowerPath, ".m4s")) {
		if initBytes := h.fetchInitSegment(r, outReq.URL.RawQuery); len(initBytes) > 0 {
			bodyReader = io.MultiReader(bytes.NewReader(initBytes), resp.Body)
		}
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

	tmpFile, err := h.storage.CreateTempFile()
	if err != nil {
		log.Printf("[JellyProxy] Warning: failed to create temp cache file: %v", err)
		if isMediaRange {
			h.proxy.ServeHTTP(w, r)
			return
		}
		for k, vals := range resp.Header {
			if restartedEncoding && strings.EqualFold(k, "Content-Length") {
				continue
			}
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-JellyProxy-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, bodyReader)
		return
	}
	tmpPath := tmpFile.Name()

	if isMediaRange {
		// keepOnClientAbort=false: if a range client disconnects mid-download (ctx cancelled),
		// abort the upstream fetch rather than continuing to download an object nobody is waiting on.
		copyErr := h.teeStream(r.Context(), io.Discard, tmpFile, bodyReader, true, false, fetch, itemID, clientID, clientName)
		closeErr := tmpFile.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(tmpPath)
			http.Error(w, "JellyProxy: Failed to cache upstream media", http.StatusBadGateway)
			return
		}
		if err := h.storage.CommitTempFile(tmpPath, meta); err != nil {
			log.Printf("[JellyProxy] Warning: failed to commit cache file for %s: %v", r.URL.Path, err)
			http.Error(w, "JellyProxy: Failed to commit cache file", http.StatusBadGateway)
			return
		}
		file, committedMeta, err := h.storage.Get(keyHash)
		if err != nil {
			http.Error(w, "JellyProxy: Failed to read cached media", http.StatusBadGateway)
			return
		}
		defer file.Close()

		for k, vals := range committedMeta.Headers {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		if committedMeta.ContentType != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", committedMeta.ContentType)
		}
		if committedMeta.ETag != "" && w.Header().Get("ETag") == "" {
			w.Header().Set("ETag", committedMeta.ETag)
		}
		if committedMeta.LastModified != "" && w.Header().Get("Last-Modified") == "" {
			w.Header().Set("Last-Modified", committedMeta.LastModified)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-JellyProxy-Cache", "MISS")

		out := &meteredWriter{ResponseWriter: w, onWrite: func(n int) {
			h.hub.Sent(clientID, clientName, itemID, n, false)
		}}
		http.ServeContent(out, r, "media", committedMeta.CreatedAt, file)
		return
	}

	for k, vals := range resp.Header {
		if restartedEncoding && strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-JellyProxy-Cache", "MISS")
	w.WriteHeader(http.StatusOK)

	// Stream to the client while teeing to disk. Only media chunks are paced and
	// accounted: they are the traffic that consumes the home upload.
	// For subtitle extractions, continue writing to disk even if the client disconnects
	// so the next retry is an instant cache HIT instead of restarting ffmpeg on a 30GB MKV.
	copyErr := h.teeStream(r.Context(), w, tmpFile, bodyReader, category == cache.CategoryMedia, isSub, fetch, itemID, clientID, clientName)
	closeErr := tmpFile.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		return
	}

	if err := h.storage.CommitTempFile(tmpPath, meta); err != nil {
		log.Printf("[JellyProxy] Warning: failed to commit cache file for %s: %v", r.URL.Path, err)
	}
}

// teeStream copies src to both the client and the cache file in paced chunks.
func (h *ReverseProxyHandler) teeStream(ctx context.Context, client io.Writer, disk io.Writer, src io.Reader,
	media bool, keepOnClientAbort bool, fetch *abr.Fetch, itemID, clientID, clientName string) error {
	flusher, _ := client.(http.Flusher)
	buf := make([]byte, streamChunk)
	clientGone := false
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if media {
				h.paceAndCount(ctx, fetch, itemID, n)
			}
			if _, err := disk.Write(buf[:n]); err != nil {
				return err
			}
			if !clientGone {
				if _, err := client.Write(buf[:n]); err != nil {
					if !keepOnClientAbort {
						return err
					}
					clientGone = true
				} else {
					if media && client != io.Discard {
						h.hub.Sent(clientID, clientName, itemID, n, false)
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
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

func (h *ReverseProxyHandler) applyUpstreamQuery(outReq *http.Request, rawQuery string) {
	targetQuery := h.upstreamURL.RawQuery
	if targetQuery == "" || rawQuery == "" {
		outReq.URL.RawQuery = targetQuery + rawQuery
	} else {
		outReq.URL.RawQuery = targetQuery + "&" + rawQuery
	}
}

// stopUpstreamEncoding calls DELETE /Videos/ActiveEncodings on Jellyfin so an
// in-flight ffmpeg process is cleanly stopped before restarting at a new bitrate.
func (h *ReverseProxyHandler) stopUpstreamEncoding(r *http.Request, rq url.Values, playSessionID string) {
	if playSessionID == "" {
		playSessionID = rq.Get("PlaySessionId")
		if playSessionID == "" {
			playSessionID = rq.Get("playSessionId")
		}
	}
	deviceID := rq.Get("DeviceId")
	if deviceID == "" {
		deviceID = rq.Get("deviceId")
	}
	if deviceID == "" {
		deviceID, _ = clientIdentity(r)
	}
	if playSessionID == "" || deviceID == "" {
		return
	}
	apiKey := rq.Get("ApiKey")
	if apiKey == "" {
		apiKey = rq.Get("api_key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	delURL := *h.upstreamURL
	delURL.Path = singleJoiningSlash(h.upstreamURL.Path, "/Videos/ActiveEncodings")
	q := url.Values{}
	q.Set("deviceId", deviceID)
	q.Set("playSessionId", playSessionID)
	if apiKey != "" {
		q.Set("ApiKey", apiKey)
	}
	delURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL.String(), nil)
	if err != nil {
		return
	}
	h.copyUpstreamAuthAndForwardHeaders(r, req, apiKey)
	if resp, err := h.httpClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// fetchInitSegment retrieves the fMP4 initialization segment (-1.mp4) written alongside
// a newly restarted ffmpeg segment so MSE updates its moov/hvcC/avcC parameters inline.
// Keeping the active segment's runtimeTicks and PlaySessionId ensures Jellyfin serves the
// freshly written -1.mp4 from disk instead of restarting ffmpeg at t=0.
func (h *ReverseProxyHandler) fetchInitSegment(r *http.Request, rawQuery string) []byte {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	initURL := *h.upstreamURL
	initURL.Path = singleJoiningSlash(h.upstreamURL.Path, path.Join(path.Dir(r.URL.Path), "-1.mp4"))
	initURL.RawQuery = rawQuery

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, initURL.String(), nil)
	if err != nil {
		return nil
	}
	var apiKey string
	if q, err := url.ParseQuery(rawQuery); err == nil {
		apiKey = q.Get("ApiKey")
		if apiKey == "" {
			apiKey = q.Get("api_key")
		}
	}
	h.copyUpstreamAuthAndForwardHeaders(r, req, apiKey)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return data
}

func (h *ReverseProxyHandler) copyUpstreamAuthAndForwardHeaders(r *http.Request, req *http.Request, apiKey string) {
	if h.cfg.UpstreamHost != "" {
		req.Host = h.cfg.UpstreamHost
	} else {
		req.Host = h.upstreamURL.Host
	}
	for _, k := range []string{"Authorization", "X-Emby-Token", "X-Emby-Authorization", "X-Forwarded-For", "X-Forwarded-Proto"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		if r.TLS != nil || strings.EqualFold(h.upstreamURL.Scheme, "https") {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}
	if apiKey != "" {
		if req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", `MediaBrowser Token="`+apiKey+`"`)
		}
		if req.Header.Get("X-Emby-Token") == "" {
			req.Header.Set("X-Emby-Token", apiKey)
		}
	}
}

// serveBitrateTest handles GET /System/BitrateTest directly on the VPS so client
// auto-bitrate probes cost 0 home upload and never mismeasure tunnel jitter.
func (h *ReverseProxyHandler) serveBitrateTest(w http.ResponseWriter, r *http.Request) {
	size, _ := strconv.Atoi(r.URL.Query().Get("Size"))
	if size <= 0 {
		size = 500_000
	}
	if size > 10_000_000 {
		size = 10_000_000
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 32*1024)
	for remaining := size; remaining > 0; {
		n := len(buf)
		if remaining < n {
			n = remaining
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return
		}
		remaining -= n
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
