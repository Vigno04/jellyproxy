package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/telemetry"
)

func testDash(t *testing.T) (*Dashboard, *http.ServeMux, *abr.BandwidthManager, *cache.StorageManager) {
	t.Helper()
	cfg := &config.Config{SharedSecret: "pw", CacheDir: t.TempDir(), MaxStorageGB: 50}
	abrMgr := abr.NewBandwidthManager(config.DefaultABRConfig())
	storageMgr, err := cache.NewStorageManager(cfg.CacheDir, cfg.MaxStorageGB)
	if err != nil {
		t.Fatal(err)
	}
	_ = config.NewSettingsStore(cfg, abrMgr, storageMgr)
	d := New(cfg, Options{
		ABR:     abrMgr,
		Storage: storageMgr,
		Hub:     telemetry.NewHub(5 * time.Second),
		Names:   telemetry.NewItemNames(),
	})
	mux := http.NewServeMux()
	d.Mount(mux, "/proxy")
	return d, mux, abrMgr, storageMgr
}

func do(mux *http.ServeMux, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestStateRequiresLogin(t *testing.T) {
	_, mux, _, _ := testDash(t)
	if w := do(mux, httptest.NewRequest("GET", "/proxy/api/state", nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated state: %d", w.Code)
	}
	w := do(mux, httptest.NewRequest("GET", "/proxy/", nil))
	if !strings.Contains(w.Body.String(), `action="login"`) {
		t.Fatal("anonymous visitors must see the login page")
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("CSP header missing")
	}
}

func TestLoginFlowAndSettingsUpdate(t *testing.T) {
	_, mux, abrMgr, storageMgr := testDash(t)

	bad := httptest.NewRequest("POST", "/proxy/login", strings.NewReader(url.Values{"password": {"nope"}}.Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if w := do(mux, bad); len(w.Result().Cookies()) != 0 {
		t.Fatal("wrong password must not set a cookie")
	}

	good := httptest.NewRequest("POST", "/proxy/login", strings.NewReader(url.Values{"password": {"pw"}}.Encode()))
	good.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := do(mux, good)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("bad session cookie: %+v", cookies)
	}

	r := httptest.NewRequest("GET", "/proxy/api/state", nil)
	r.AddCookie(cookies[0])
	w = do(mux, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"live"`) {
		t.Fatalf("state with cookie: %d %s", w.Code, w.Body.String())
	}

	var state State
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || state.CSRFToken == "" {
		t.Fatalf("expected csrf token in state: %v", err)
	}

	// Update settings without CSRF token -> 403 Forbidden
	noCSRF := httptest.NewRequest("POST", "/proxy/api/settings", strings.NewReader(`{"maxStorageGb":25}`))
	noCSRF.AddCookie(cookies[0])
	if w := do(mux, noCSRF); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf token, got %d", w.Code)
	}

	// Update settings with valid CSRF token -> 200 OK
	settingsPayload := `{"abr":{"enabled":true,"uploadSpeedBps":24000000,"headroomPercent":15,"policy":"FairShare","minBitrateBps":2000000,"singleClientCeilingBps":12000000,"allowDegradingExistingStreams":true,"fastStartSeconds":8,"fastStartSharePercent":85,"seekBorrowSeconds":5,"backgroundFloorPercent":10},"encoders":["h264","hevc","av1"],"maxStorageGb":25}`
	withCSRF := httptest.NewRequest("POST", "/proxy/api/settings", strings.NewReader(settingsPayload))
	withCSRF.AddCookie(cookies[0])
	withCSRF.Header.Set("X-CSRF-Token", state.CSRFToken)
	if w := do(mux, withCSRF); w.Code != http.StatusOK {
		t.Fatalf("expected 200 on settings save, got %d: %s", w.Code, w.Body.String())
	}

	if got := abrMgr.Config().UploadSpeedBps; got != 24_000_000 {
		t.Fatalf("expected UploadSpeedBps 24M, got %d", got)
	}
	if got := abrMgr.Capabilities().Encoders(); len(got) != 3 {
		t.Fatalf("expected 3 encoders, got %v", got)
	}
	if got := storageMgr.GetStats().MaxStorageGB; got != 25 {
		t.Fatalf("expected MaxStorageGB 25, got %v", got)
	}

	forged := httptest.NewRequest("GET", "/proxy/api/state", nil)
	forged.AddCookie(&http.Cookie{Name: cookieNameHTTP, Value: "9999999999.deadbeef"})
	if w := do(mux, forged); w.Code != http.StatusUnauthorized {
		t.Fatal("forged cookie accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	_, mux, _, _ := testDash(t)
	var last int
	for i := 0; i < maxFailures+1; i++ {
		r := httptest.NewRequest("POST", "/proxy/login", strings.NewReader("password=bad"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		last = do(mux, r).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected lockout, got %d", last)
	}
}

func TestResolveBasePathSkipsTakenRoute(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/proxy") {
			w.WriteHeader(http.StatusOK) // Jellyfin (or the admin's own app) owns /proxy
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	cfg := &config.Config{UpstreamURL: up.URL, SharedSecret: "pw"}
	if got := ResolveBasePath(cfg); got != "/jellyproxy" {
		t.Fatalf("expected fallback /jellyproxy, got %q", got)
	}

	free := httptest.NewServer(http.NotFoundHandler())
	defer free.Close()
	cfg.UpstreamURL = free.URL
	if got := ResolveBasePath(cfg); got != "/proxy" {
		t.Fatalf("expected /proxy, got %q", got)
	}

	cfg.SharedSecret = ""
	if got := ResolveBasePath(cfg); got != "" {
		t.Fatal("no secret: dashboard must stay off")
	}
}
