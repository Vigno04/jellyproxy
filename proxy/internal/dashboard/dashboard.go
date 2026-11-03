// Package dashboard serves JellyProxy's built-in web UI: a live topology drawing of
// home server, proxy and clients, plus cache, shaper and session statistics and
// runtime configuration controls.
//
// The UI is mounted under /proxy on the same listener that proxies Jellyfin. Before
// mounting, the upstream is probed so the dashboard never shadows a route Jellyfin
// already owns.
package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/playback"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/preseed"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/telemetry"
)

//go:embed static
var staticFiles embed.FS

const (
	cookieNameHTTP  = "jp_dash"
	cookieNameHTTPS = "__Host-jp_dash"
	sessionTTL      = 12 * time.Hour
	maxFailures     = 5
	failureLockT    = 10 * time.Minute
	defaultBasePath = "/proxy"
)

// SessionSource lists live playback sessions.
type SessionSource interface{ Snapshot() []playback.SessionInfo }

// JobController lists, starts and cancels pre-seeding jobs.
type JobController interface {
	Jobs() []*preseed.Job
	StartWithOptions(id, itemName string, urls []string, headers http.Header) error
	Cancel(id string) bool
}

// StatusSource reports the pairing state with the home plugin.
type StatusSource interface{ StatusSnapshot() map[string]any }

// Options carries every data source and controller the dashboard uses.
type Options struct {
	ABR      *abr.BandwidthManager
	Storage  *cache.StorageManager
	Hub      *telemetry.Hub
	Names    *telemetry.ItemNames
	Sessions SessionSource
	Jobs     JobController
	Status   StatusSource
}

// Dashboard is the HTTP handler set of the UI.
// TODO(security): Consider OAuth/OIDC, MFA, and leaked-password checks if multi-user accounts are added in the future.
type Dashboard struct {
	cfg  *config.Config
	opts Options
	key  []byte

	mu       sync.Mutex
	failures map[string]*failure
	revoked  map[string]int64
}

type failure struct {
	count int
	until time.Time
}

// New creates a dashboard authenticated with cfg.SharedSecret.
func New(cfg *config.Config, opts Options) *Dashboard {
	var keyMaterial []byte
	if cfg.SharedSecret != "" {
		sum := sha256.Sum256([]byte("jellyproxy-dashboard:" + cfg.SharedSecret))
		keyMaterial = sum[:]
	} else {
		// Ephemeral random key when no secret is configured (dashboard stays disabled until secret is set).
		keyMaterial = make([]byte, 32)
		_, _ = rand.Read(keyMaterial)
	}
	return &Dashboard{
		cfg:      cfg,
		opts:     opts,
		key:      keyMaterial,
		failures: map[string]*failure{},
		revoked:  map[string]int64{},
	}
}

// ResolveBasePath returns the path the dashboard should be mounted at, or "" when it
// must stay off (no shared secret configured, or every candidate is already used by Jellyfin).
func ResolveBasePath(cfg *config.Config) string {
	if cfg.SharedSecret == "" {
		log.Printf("[JellyProxy] Dashboard disabled: set SHARED_SECRET to enable it")
		return ""
	}
	client := &http.Client{
		Timeout:       4 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // private tunnel, self-signed
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, candidate := range []string{defaultBasePath, "/jellyproxy"} {
		switch probe(client, cfg, candidate) {
		case probeFree:
			log.Printf("[JellyProxy] Dashboard available at %s/", candidate)
			return candidate
		case probeTaken:
			log.Printf("[JellyProxy] Dashboard path %s is already used by Jellyfin, trying next", candidate)
		}
	}
	log.Printf("[JellyProxy] Dashboard disabled: no free path on the upstream")
	return ""
}

type probeResult int

const (
	probeFree probeResult = iota
	probeTaken
)

// probe asks the upstream whether path is already served. Jellyfin answers 404 for
// unknown paths; any other status means the path is taken.
func probe(client *http.Client, cfg *config.Config, path string) probeResult {
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, cfg.UpstreamURL+path+"/", nil)
		if err != nil {
			return probeFree
		}
		if cfg.UpstreamHost != "" {
			req.Host = cfg.UpstreamHost
		}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return probeFree
		}
		return probeTaken
	}
	return probeFree
}

// Mount registers the UI routes under base (for example "/proxy").
func (d *Dashboard) Mount(mux *http.ServeMux, base string) {
	assets, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(assets))

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+"/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc(base+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		setSecurityHeaders(w)
		rest := strings.TrimPrefix(r.URL.Path, base+"/")
		switch rest {
		case "":
			page := "login.html"
			if _, ok := d.sessionToken(r); ok {
				page = "index.html"
			}
			w.Header().Set("Cache-Control", "no-store")
			serveFile(w, assets, page)
		case "app.js", "style.css":
			r.URL.Path = "/" + rest
			files.ServeHTTP(w, r)
		case "login":
			d.handleLogin(w, r, base)
		case "logout":
			d.handleLogout(w, r, base)
		case "api/state":
			tok, ok := d.sessionToken(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			d.handleState(w, r, tok)
		case "api/settings":
			if !d.requireWriteAuth(w, r) {
				return
			}
			d.handleSettings(w, r)
		case "api/cache/purge":
			if !d.requireWriteAuth(w, r) {
				return
			}
			d.handlePurge(w, r)
		case "api/preseed/start":
			if !d.requireWriteAuth(w, r) {
				return
			}
			d.handlePreseedStart(w, r)
		case "api/preseed/cancel":
			if !d.requireWriteAuth(w, r) {
				return
			}
			d.handlePreseedCancel(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveFile(w http.ResponseWriter, fsys fs.FS, name string) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// --- authentication & CSRF ------------------------------------------------------

func (d *Dashboard) sign(payload string) string {
	mac := hmac.New(sha256.New, d.key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Dashboard) csrfFor(sessionPayload string) string {
	return d.sign("csrf:" + sessionPayload)
}

func cookieNameFor(r *http.Request) string {
	if secure(r) {
		return cookieNameHTTPS
	}
	return cookieNameHTTP
}

func (d *Dashboard) sessionToken(r *http.Request) (string, bool) {
	var raw string
	for _, name := range []string{cookieNameHTTPS, cookieNameHTTP} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			raw = c.Value
			break
		}
	}
	if raw == "" {
		return "", false
	}
	lastDot := strings.LastIndex(raw, ".")
	if lastDot <= 0 {
		return "", false
	}
	payload, sig := raw[:lastDot], raw[lastDot+1:]
	if !hmac.Equal([]byte(sig), []byte(d.sign(payload))) {
		return "", false
	}
	// payload is either "<sid>.<expiry>" or legacy "<expiry>"
	expStr := payload
	if dot := strings.LastIndex(payload, "."); dot >= 0 {
		expStr = payload[dot+1:]
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	d.mu.Lock()
	_, isRevoked := d.revoked[payload]
	d.mu.Unlock()
	if isRevoked {
		return "", false
	}
	return payload, true
}

func (d *Dashboard) authenticated(r *http.Request) bool {
	_, ok := d.sessionToken(r)
	return ok
}

func (d *Dashboard) requireWriteAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	tok, ok := d.sessionToken(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if csrf == "" {
		csrf = r.URL.Query().Get("csrf")
	}
	expected := d.csrfFor(tok)
	if !hmac.Equal([]byte(csrf), []byte(expected)) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return false
	}
	return true
}

func clientKey(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	return r.RemoteAddr
}

func (d *Dashboard) locked(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.failures[key]
	return f != nil && f.count >= maxFailures && time.Now().Before(f.until)
}

func (d *Dashboard) recordFailure(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.failures[key]
	if f == nil || time.Now().After(f.until) {
		f = &failure{}
		d.failures[key] = f
	}
	f.count++
	f.until = time.Now().Add(failureLockT)
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request, base string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := clientKey(r)
	if d.locked(key) {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a, b := sha256.Sum256([]byte(r.PostForm.Get("password"))), sha256.Sum256([]byte(d.cfg.SharedSecret))
	if d.cfg.SharedSecret == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		d.recordFailure(key)
		http.Redirect(w, r, base+"/?error=1", http.StatusSeeOther)
		return
	}
	sidBytes := make([]byte, 16)
	_, _ = rand.Read(sidBytes)
	sid := hex.EncodeToString(sidBytes)
	expiry := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	payload := sid + "." + expiry
	http.SetCookie(w, &http.Cookie{
		Name:     cookieNameFor(r),
		Value:    payload + "." + d.sign(payload),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, base+"/", http.StatusSeeOther)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request, base string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tok, ok := d.sessionToken(r); ok {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		_ = r.ParseForm()
		csrf := r.PostForm.Get("csrf_token")
		if csrf == "" {
			csrf = r.Header.Get("X-CSRF-Token")
		}
		if csrf != "" && !hmac.Equal([]byte(csrf), []byte(d.csrfFor(tok))) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		d.mu.Lock()
		d.revoked[tok] = time.Now().Add(sessionTTL).Unix()
		d.mu.Unlock()
	}
	for _, name := range []string{cookieNameHTTPS, cookieNameHTTP} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   secure(r),
			SameSite: http.SameSiteStrictMode,
		})
	}
	http.Redirect(w, r, base+"/", http.StatusSeeOther)
}

// --- state & settings APIs ------------------------------------------------------

// State is the single JSON document the UI polls once per second.
type State struct {
	Now       time.Time              `json:"now"`
	CSRFToken string                 `json:"csrfToken,omitempty"`
	Live      telemetry.Snapshot     `json:"live"`
	ABR       abr.Status             `json:"abr"`
	Encoders  []string               `json:"encoders"`
	Storage   *cache.StorageStats    `json:"storage,omitempty"`
	Sessions  []playback.SessionInfo `json:"sessions"`
	Jobs      []*preseed.Job         `json:"jobs"`
	Server    map[string]any         `json:"server,omitempty"`
}

// Collect gathers the current state from every source.
func (d *Dashboard) Collect() State {
	s := State{
		Now:      time.Now(),
		Encoders: []string{},
		Sessions: []playback.SessionInfo{},
		Jobs:     []*preseed.Job{},
	}
	if d.opts.Hub != nil {
		s.Live = d.opts.Hub.Snapshot(d.opts.Names)
	}
	if d.opts.ABR != nil {
		s.ABR = d.opts.ABR.GetStatus()
		if enc := d.opts.ABR.Capabilities().Encoders(); len(enc) > 0 {
			s.Encoders = enc
		}
	}
	if d.opts.Storage != nil {
		st := d.opts.Storage.GetStats()
		s.Storage = &st
	}
	if d.opts.Sessions != nil {
		s.Sessions = d.opts.Sessions.Snapshot()
	}
	if d.opts.Jobs != nil {
		s.Jobs = d.opts.Jobs.Jobs()
	}
	if d.opts.Status != nil {
		s.Server = d.opts.Status.StatusSnapshot()
		delete(s.Server, "upstream_url") // internal tunnel address, no need to expose it
	}
	return s
}

func (d *Dashboard) handleState(w http.ResponseWriter, r *http.Request, sessionPayload string) {
	st := d.Collect()
	st.CSRFToken = d.csrfFor(sessionPayload)
	writeJSONResponse(w, http.StatusOK, st)
}

type settingsRequest struct {
	ABR          *abr.Config `json:"abr"`
	Encoders     []string    `json:"encoders"`
	MaxStorageGB float64     `json:"maxStorageGb"`
}

func (d *Dashboard) handleSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req settingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if d.opts.ABR != nil {
		if req.ABR != nil {
			d.opts.ABR.UpdateConfig(*req.ABR)
		}
		if req.Encoders != nil {
			d.opts.ABR.Capabilities().SetEncoders(req.Encoders)
		}
	}
	if d.opts.Storage != nil && req.MaxStorageGB > 0 {
		if err := d.opts.Storage.SetMaxStorageGB(req.MaxStorageGB); err != nil {
			http.Error(w, "invalid storage quota", http.StatusBadRequest)
			return
		}
		d.cfg.MaxStorageGB = req.MaxStorageGB
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (d *Dashboard) handlePurge(w http.ResponseWriter, r *http.Request) {
	if d.opts.Storage == nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Category string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	cat := strings.ToLower(strings.TrimSpace(req.Category))
	if cat == "" {
		cat = cache.CategoryAll
	}
	removed, err := d.opts.Storage.Purge(cat)
	if err != nil {
		http.Error(w, "invalid purge category", http.StatusBadRequest)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"status": "ok", "removed": removed})
}

func (d *Dashboard) handlePreseedStart(w http.ResponseWriter, r *http.Request) {
	if d.opts.Jobs == nil {
		http.Error(w, "preseed unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req struct {
		ID       string   `json:"id"`
		ItemName string   `json:"itemName"`
		URLs     []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.URLs) == 0 {
		http.Error(w, "urls are required", http.StatusBadRequest)
		return
	}
	if err := d.opts.Jobs.StartWithOptions(req.ID, req.ItemName, req.URLs, nil); err != nil {
		http.Error(w, "job already running or invalid", http.StatusConflict)
		return
	}
	writeJSONResponse(w, http.StatusAccepted, map[string]any{"status": "started"})
}

func (d *Dashboard) handlePreseedCancel(w http.ResponseWriter, r *http.Request) {
	if d.opts.Jobs == nil {
		http.Error(w, "preseed unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !d.opts.Jobs.Cancel(req.ID) {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"status": "cancelled"})
}

func writeJSONResponse(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("[JellyProxy] dashboard json encode error")
	}
}
