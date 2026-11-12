package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/telemetry"
)

func TestPreferTextSubtitleDelivery(t *testing.T) {
	body := map[string]any{"DeviceProfile": map[string]any{"SubtitleProfiles": []any{
		map[string]any{"Format": "ass", "Method": "Encode"},
		map[string]any{"Format": "pgssub", "Method": "Encode"},
		map[string]any{"Format": "srt", "Method": "External"},
	}}}
	if !preferTextSubtitleDelivery(body) {
		t.Fatal("profile should have been rewritten")
	}
	list := body["DeviceProfile"].(map[string]any)["SubtitleProfiles"].([]any)
	var hasVTT, hasASSEncode, hasPGSEncode bool
	for _, e := range list {
		p := e.(map[string]any)
		f, m := p["Format"], p["Method"]
		hasVTT = hasVTT || (f == "vtt" && m == "External")
		hasASSEncode = hasASSEncode || (f == "ass" && m == "Encode")
		hasPGSEncode = hasPGSEncode || (f == "pgssub" && m == "Encode")
	}
	if !hasVTT || hasASSEncode || !hasPGSEncode {
		t.Fatalf("vtt=%v assEncode=%v pgsEncode=%v", hasVTT, hasASSEncode, hasPGSEncode)
	}
	if preferTextSubtitleDelivery(map[string]any{}) {
		t.Fatal("no DeviceProfile means nothing to rewrite")
	}
}

func TestBurnsInSubtitles(t *testing.T) {
	yes := `{"MediaSources":[{"TranscodingUrl":"/videos/1/master.m3u8?SubtitleMethod=Encode&SubtitleStreamIndex=3&x=1"}]}`
	noIdx := `{"MediaSources":[{"TranscodingUrl":"/videos/1/master.m3u8?SubtitleMethod=Encode&x=1"}]}`
	no := `{"MediaSources":[{"TranscodingUrl":"/videos/1/master.m3u8?SubtitleMethod=External"}]}`
	if !burnsInSubtitles([]byte(yes)) || burnsInSubtitles([]byte(noIdx)) || burnsInSubtitles([]byte(no)) || burnsInSubtitles([]byte("junk")) {
		t.Fatal("burn-in detection wrong")
	}
}

func TestFilterVariants(t *testing.T) {
	pl := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=8000000\nhigh.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=4000000\nmid.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nlow.m3u8\n"
	out := string(filterVariants([]byte(pl), 5_000_000))
	if strings.Contains(out, "high.m3u8") || !strings.Contains(out, "mid.m3u8") || !strings.Contains(out, "low.m3u8") {
		t.Fatalf("unexpected playlist:\n%s", out)
	}
	out = string(filterVariants([]byte(pl), 100))
	if !strings.Contains(out, "low.m3u8") || strings.Contains(out, "mid.m3u8") {
		t.Fatalf("lowest variant must always survive:\n%s", out)
	}
	if string(filterVariants([]byte(pl), 0)) != pl {
		t.Fatal("no ceiling: playlist untouched")
	}
}

func TestClientIdentity(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?DeviceId=abc", nil)
	r.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="zzz", Version="10"`)
	if id, name := clientIdentity(r); id != "abc" || name != "Firefox" {
		t.Fatalf("got %q %q", id, name)
	}
	r = httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "1.2.3.4:5555"
	if id, _ := clientIdentity(r); id != "1.2.3.4" {
		t.Fatalf("fallback id %q", id)
	}
}

func TestIsDirectStream(t *testing.T) {
	for p, want := range map[string]bool{
		"/Videos/abc/stream":        true,
		"/Videos/abc/stream.mkv":    true,
		"/Items/abc/Download":       true,
		"/Videos/abc/master.m3u8":   false,
		"/Items/abc/Images/Primary": false,
	} {
		if got := isDirectStream(p); got != want {
			t.Errorf("isDirectStream(%q)=%v", p, got)
		}
	}
}

// fakeJellyfin counts how many media segment requests actually reach "home".
func fakeJellyfin(t *testing.T, segHits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/videos/movie1/hls1/main/0.m4s", func(w http.ResponseWriter, r *http.Request) {
		*segHits++
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
	})
	mux.HandleFunc("/Items/movie1/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b) // echo what the proxy sent
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestHandler(t *testing.T, upstream string) (*ReverseProxyHandler, *abr.BandwidthManager, *telemetry.Hub) {
	t.Helper()
	cfg := &config.Config{UpstreamURL: upstream}
	store, err := cache.NewStorageManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewReverseProxyHandler(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	m := abr.NewBandwidthManager(abr.Config{Enabled: true, UploadSpeedBps: 100_000_000})
	hub := telemetry.NewHub(5e9)
	h.SetABR(m)
	h.SetTelemetry(hub, telemetry.NewItemNames())
	return h, m, hub
}

// Case 9: the second viewer, starting later, is served from the VPS cache.
func TestTimeShiftedViewerCostsNoUpload(t *testing.T) {
	hits := 0
	up := fakeJellyfin(t, &hits)
	h, _, hub := newTestHandler(t, up.URL)

	get := func(device string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/videos/movie1/hls1/main/0.m4s?DeviceId="+device+"&PlaySessionId=s-"+device, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	first, second := get("tv"), get("phone")
	if first.Header().Get("X-JellyProxy-Cache") != "MISS" || second.Header().Get("X-JellyProxy-Cache") != "HIT" {
		t.Fatalf("cache headers: %q then %q", first.Header().Get("X-JellyProxy-Cache"), second.Header().Get("X-JellyProxy-Cache"))
	}
	if hits != 1 || second.Body.Len() != 1024 {
		t.Fatalf("home was hit %d times, body %d", hits, second.Body.Len())
	}
	snap := hub.Snapshot(nil)
	if snap.TotalPulled != 1024 || snap.TotalOut != 2048 {
		t.Fatalf("telemetry: pulled=%d out=%d", snap.TotalPulled, snap.TotalOut)
	}
}

func TestPlaybackInfoIsShapedAndSubtitlesRewritten(t *testing.T) {
	hits := 0
	up := fakeJellyfin(t, &hits)
	h, m, _ := newTestHandler(t, up.URL)
	m.UpdateConfig(abr.Config{Enabled: true, UploadSpeedBps: 10_000_000})

	body := `{"MaxStreamingBitrate":80000000,"DeviceProfile":{"TranscodingProfiles":[{"Type":"Video","VideoCodec":"h264,hevc"}],"SubtitleProfiles":[{"Format":"ass","Method":"Encode"}]}}`
	r := httptest.NewRequest("POST", "/Items/movie1/PlaybackInfo?DeviceId=tv", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var sent map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &sent); err != nil {
		t.Fatalf("echo not JSON: %v (%s)", err, w.Body.String())
	}
	if got := int64(sent["MaxStreamingBitrate"].(float64)); got != 10_000_000 {
		t.Fatalf("bitrate should be capped to X, got %d", got)
	}
	prof := sent["DeviceProfile"].(map[string]any)
	if vc := prof["TranscodingProfiles"].([]any)[0].(map[string]any)["VideoCodec"]; vc != "hevc" {
		t.Fatalf("codec should converge on hevc, got %v", vc)
	}
	for _, e := range prof["SubtitleProfiles"].([]any) {
		if e.(map[string]any)["Method"] == "Encode" {
			t.Fatal("text subtitle burn-in must be removed")
		}
	}
}
