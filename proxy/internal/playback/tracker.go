package playback

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
	"github.com/Vigno04/jellyproxy/proxy/internal/auth"
	"github.com/Vigno04/jellyproxy/proxy/internal/cache"
	"github.com/Vigno04/jellyproxy/proxy/internal/config"
	"github.com/Vigno04/jellyproxy/proxy/internal/telemetry"
)

type PlaybackReport struct {
	Secret              string  `json:"secret"`
	SessionID           string  `json:"session_id"`
	DeviceID            string  `json:"device_id"`
	DeviceName          string  `json:"device_name"`
	ClientName          string  `json:"client_name"`
	UserID              string  `json:"user_id"`
	UserName            string  `json:"user_name"`
	ItemID              string  `json:"item_id"`
	ItemName            string  `json:"item_name"`
	NextItemID          string  `json:"next_item_id,omitempty"`
	NextItemName        string  `json:"next_item_name,omitempty"`
	PlaySessionID       string  `json:"play_session_id"`
	PositionTicks       int64   `json:"position_ticks"`
	TotalTicks          int64   `json:"total_ticks"`
	IsPaused            bool    `json:"is_paused"`
	EventType           string  `json:"event_type"`
	PlayMethod          string  `json:"play_method"`
	VideoCodec          string  `json:"video_codec,omitempty"`
	AudioCodec          string  `json:"audio_codec,omitempty"`
	TranscodeBitrate    int64   `json:"transcode_bitrate,omitempty"`
	TranscodeCompletion float64 `json:"transcode_completion,omitempty"`
	AudioStreamIndex    string  `json:"audio_stream_index,omitempty"`
	SubtitleStreamIndex string  `json:"subtitle_stream_index,omitempty"`
	MediaSourceID       string  `json:"media_source_id,omitempty"`
}

type SessionInfo struct {
	SessionID           string            `json:"session_id"`
	DeviceID            string            `json:"device_id"`
	DeviceName          string            `json:"device_name"`
	ClientName          string            `json:"client_name"`
	UserName            string            `json:"user_name"`
	ItemID              string            `json:"item_id"`
	ItemName            string            `json:"item_name"`
	NextItemID          string            `json:"next_item_id,omitempty"`
	NextItemName        string            `json:"next_item_name,omitempty"`
	PlaySessionID       string            `json:"play_session_id"`
	PositionTicks       int64             `json:"position_ticks"`
	TotalTicks          int64             `json:"total_ticks"`
	IsPaused            bool              `json:"is_paused"`
	PlayMethod          string            `json:"play_method"`
	VideoCodec          string            `json:"video_codec,omitempty"`
	AudioCodec          string            `json:"audio_codec,omitempty"`
	TranscodeBitrate    int64             `json:"transcode_bitrate,omitempty"`
	TranscodeCompletion float64           `json:"transcode_completion,omitempty"`
	LastSeen            time.Time         `json:"last_seen"`
	PrefetchActive      bool              `json:"prefetch_active"`
	PrefetchedCount     int               `json:"prefetched_count"`
	LastSegmentURL      string            `json:"last_segment_url,omitempty"`
	Segments            []string          `json:"-"`
	AuthHeaders         map[string]string `json:"-"`
	cancelPrefetch      context.CancelFunc
}

type SessionTracker struct {
	cfg            *config.Config
	storage        *cache.StorageManager
	client         *http.Client
	sessions       map[string]*SessionInfo
	playSessionMap map[string]string
	activeLive     *int32
	abr            *abr.BandwidthManager
	names          *telemetry.ItemNames
	mu             sync.RWMutex
}

func NewSessionTracker(cfg *config.Config, storage *cache.StorageManager, activeLive *int32, shaper *abr.BandwidthManager, names *telemetry.ItemNames) *SessionTracker {
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     60 * time.Second,
	}

	st := &SessionTracker{
		cfg:            cfg,
		storage:        storage,
		client:         &http.Client{Transport: transport, Timeout: 15 * time.Second},
		sessions:       make(map[string]*SessionInfo),
		playSessionMap: make(map[string]string),
		activeLive:     activeLive,
		abr:            shaper,
		names:          names,
	}

	go st.cleanupLoop()
	return st
}

func (st *SessionTracker) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		st.mu.Lock()
		now := time.Now()
		for id, sess := range st.sessions {
			if now.Sub(sess.LastSeen) > 60*time.Second {
				if sess.cancelPrefetch != nil {
					sess.cancelPrefetch()
				}
				if sess.PlaySessionID != "" {
					delete(st.playSessionMap, sess.PlaySessionID)
				}
				delete(st.sessions, id)
			}
		}
		st.mu.Unlock()
	}
}

func (st *SessionTracker) IngestPlaylist(playlistURL string, body string, headers http.Header) {
	st.mu.Lock()
	defer st.mu.Unlock()

	parsed, err := url.Parse(playlistURL)
	if err != nil {
		return
	}
	baseDir := path.Dir(parsed.Path)
	playSessionID := parsed.Query().Get("PlaySessionId")

	var sess *SessionInfo
	if playSessionID != "" {
		if sid, ok := st.playSessionMap[playSessionID]; ok {
			sess = st.sessions[sid]
		}
	}

	segments := make([]string, 0)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:URI=\"") {
			uri := strings.TrimPrefix(line, "#EXT-X-MAP:URI=\"")
			uri = strings.TrimSuffix(uri, "\"")
			if !strings.HasPrefix(uri, "http://") && !strings.HasPrefix(uri, "https://") && !strings.HasPrefix(uri, "/") {
				uri = path.Join(baseDir, uri)
			}
			segments = append(segments, uri)
		} else if !strings.HasPrefix(line, "#") {
			segURI := line
			if !strings.HasPrefix(segURI, "http://") && !strings.HasPrefix(segURI, "https://") && !strings.HasPrefix(segURI, "/") {
				segURI = path.Join(baseDir, segURI)
			}
			segments = append(segments, segURI)
		}
	}

	if sess != nil {
		sess.Segments = segments
		sess.AuthHeaders = extractAuthHeaders(headers)
	}
}

func extractAuthHeaders(h http.Header) map[string]string {
	res := make(map[string]string)
	for _, key := range []string{"Authorization", "X-Emby-Token", "X-Emby-Authorization", "User-Agent"} {
		if val := h.Get(key); val != "" {
			res[key] = val
		}
	}
	return res
}

func (st *SessionTracker) OnSegmentRequested(rawURL string, headers http.Header) {
	st.mu.Lock()
	defer st.mu.Unlock()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	playSessionID := parsed.Query().Get("PlaySessionId")
	if playSessionID == "" {
		return
	}

	sid, ok := st.playSessionMap[playSessionID]
	if !ok {
		return
	}
	sess, ok := st.sessions[sid]
	if !ok {
		return
	}

	sess.LastSegmentURL = rawURL
	sess.LastSeen = time.Now()
	if len(sess.AuthHeaders) == 0 {
		sess.AuthHeaders = extractAuthHeaders(headers)
	}

	// Any client request for media chunk implies live playback: cancel prefetch immediately
	if sess.cancelPrefetch != nil {
		sess.cancelPrefetch()
		sess.cancelPrefetch = nil
		sess.PrefetchActive = false
	}
}

func (st *SessionTracker) HandleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var rep PlaybackReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if st.cfg.SharedSecret != "" && !auth.IsAuthorized(r, st.cfg.SharedSecret) {
		secret := rep.Secret
		if secret == "" {
			secret = r.Header.Get("X-JellyProxy-Secret")
		}
		if secret != st.cfg.SharedSecret {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	st.mu.Lock()
	sess, ok := st.sessions[rep.SessionID]
	if !ok {
		sess = &SessionInfo{
			SessionID: rep.SessionID,
		}
		st.sessions[rep.SessionID] = sess
	}

	sess.DeviceID = rep.DeviceID
	sess.DeviceName = rep.DeviceName
	sess.ClientName = rep.ClientName
	sess.UserName = rep.UserName
	sess.ItemID = rep.ItemID
	sess.ItemName = rep.ItemName
	sess.NextItemID = rep.NextItemID
	sess.NextItemName = rep.NextItemName
	sess.PlaySessionID = rep.PlaySessionID
	sess.PositionTicks = rep.PositionTicks
	sess.TotalTicks = rep.TotalTicks
	sess.IsPaused = rep.IsPaused
	sess.PlayMethod = rep.PlayMethod
	sess.VideoCodec = rep.VideoCodec
	sess.AudioCodec = rep.AudioCodec
	sess.TranscodeBitrate = rep.TranscodeBitrate
	sess.TranscodeCompletion = rep.TranscodeCompletion
	sess.LastSeen = time.Now()

	if rep.PlaySessionID != "" {
		st.playSessionMap[rep.PlaySessionID] = rep.SessionID
	}
	st.names.Set(rep.ItemID, rep.ItemName)
	if rep.NextItemID != "" && rep.NextItemName != "" {
		st.names.Set(rep.NextItemID, rep.NextItemName)
	}
	if st.abr != nil && rep.DeviceID != "" {
		switch {
		case rep.EventType == "stop":
			st.abr.Release(rep.DeviceID)
		case rep.EventType == "seek":
			// Spec case 12: the seeking client's buffer is gone, lend it bandwidth.
			st.abr.Seek(rep.DeviceID)
		case rep.ItemID != "":
			st.abr.Touch(rep.DeviceID, rep.ItemID)
		}
	}

	if rep.EventType == "stop" {
		if sess.cancelPrefetch != nil {
			sess.cancelPrefetch()
		}
		if sess.PlaySessionID != "" {
			delete(st.playSessionMap, sess.PlaySessionID)
		}
		delete(st.sessions, rep.SessionID)
		st.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}

	if rep.EventType == "unpause" || rep.EventType == "seek" || (!rep.IsPaused && sess.PrefetchActive) {
		if sess.cancelPrefetch != nil {
			sess.cancelPrefetch()
			sess.cancelPrefetch = nil
			sess.PrefetchActive = false
		}
	}

	if rep.EventType == "pause" || rep.IsPaused {
		if sess.cancelPrefetch != nil {
			sess.cancelPrefetch()
			sess.cancelPrefetch = nil
		}
		sess.PrefetchActive = true
		ctx, cancel := context.WithCancel(context.Background())
		sess.cancelPrefetch = cancel

		// Debounce 1.5s to prevent prefetching on quick scrub/seek
		go func(sid string, ctx context.Context) {
			select {
			case <-time.After(1500 * time.Millisecond):
				st.runPrefetch(sid, ctx)
			case <-ctx.Done():
				return
			}
		}(rep.SessionID, ctx)
	}

	st.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (st *SessionTracker) runPrefetch(sessionID string, ctx context.Context) {
	st.mu.RLock()
	sess, ok := st.sessions[sessionID]
	if !ok || !sess.IsPaused || ctx.Err() != nil {
		st.mu.RUnlock()
		return
	}
	segments := append([]string(nil), sess.Segments...)
	lastURL := sess.LastSegmentURL
	headers := make(map[string]string)
	for k, v := range sess.AuthHeaders {
		headers[k] = v
	}
	st.mu.RUnlock()

	if len(segments) == 0 {
		return
	}

	startIdx := 0
	if lastURL != "" {
		for i, seg := range segments {
			if strings.Contains(lastURL, seg) || strings.Contains(seg, lastURL) {
				startIdx = i + 1
				break
			}
		}
	}

	maxPrefetch := 10
	for i := startIdx; i < len(segments) && i < startIdx+maxPrefetch; i++ {
		if ctx.Err() != nil {
			return
		}

		segURL := segments[i]
		keyHash := cache.HashKey(cache.BuildKey(cache.CategoryMedia, segURL))
		if st.storage.Has(keyHash) {
			continue
		}

		targetURL := strings.TrimRight(st.cfg.UpstreamURL, "/") + "/" + strings.TrimLeft(segURL, "/")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return
		}
		if st.cfg.UpstreamHost != "" {
			req.Host = st.cfg.UpstreamHost
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := st.client.Do(req)
		if err != nil {
			return
		}
		if resp.StatusCode == http.StatusOK {
			tmp, err := st.storage.CreateTempFile()
			if err == nil {
				tmpPath := tmp.Name()
				// Spec case 11: prefetch yields to live playback via the background budget.
				var wait abr.WaitFunc
				if st.abr != nil {
					wait = st.abr.BackgroundWait
				}
				_, copyErr := io.Copy(tmp, abr.PacedReader(ctx, resp.Body, wait))
				_ = tmp.Close()
				_ = resp.Body.Close()

				if copyErr == nil && ctx.Err() == nil {
					meta := cache.CacheMetadata{
						Key:         keyHash,
						URL:         segURL,
						Category:    cache.CategoryMedia,
						ContentType: resp.Header.Get("Content-Type"),
						CreatedAt:   time.Now().UTC(),
					}
					if err := st.storage.CommitTempFile(tmpPath, meta); err == nil {
						st.mu.Lock()
						if s, exists := st.sessions[sessionID]; exists {
							s.PrefetchedCount++
						}
						st.mu.Unlock()
					}
				} else {
					_ = st.storage.Delete(keyHash)
				}
			} else {
				_ = resp.Body.Close()
			}
		} else {
			_ = resp.Body.Close()
		}
	}

	st.mu.Lock()
	if s, exists := st.sessions[sessionID]; exists {
		s.PrefetchActive = false
	}
	st.mu.Unlock()
}

func (st *SessionTracker) HandleSessions(w http.ResponseWriter, r *http.Request) {
	if !auth.IsAuthorized(r, st.cfg.SharedSecret) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	st.mu.RLock()
	list := make([]*SessionInfo, 0, len(st.sessions))
	for _, sess := range st.sessions {
		list = append(list, sess)
	}
	st.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// Snapshot returns a copy of the live sessions for the dashboard.
func (st *SessionTracker) Snapshot() []SessionInfo {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]SessionInfo, 0, len(st.sessions))
	for _, s := range st.sessions {
		out = append(out, SessionInfo{
			SessionID: s.SessionID, DeviceID: s.DeviceID, DeviceName: s.DeviceName, ClientName: s.ClientName,
			UserName: s.UserName, ItemID: s.ItemID, ItemName: s.ItemName, NextItemID: s.NextItemID,
			NextItemName: s.NextItemName, PositionTicks: s.PositionTicks, TotalTicks: s.TotalTicks,
			IsPaused: s.IsPaused, PlayMethod: s.PlayMethod, VideoCodec: s.VideoCodec, AudioCodec: s.AudioCodec,
			TranscodeBitrate: s.TranscodeBitrate, TranscodeCompletion: s.TranscodeCompletion,
			LastSeen: s.LastSeen, PrefetchActive: s.PrefetchActive, PrefetchedCount: s.PrefetchedCount,
		})
	}
	return out
}
