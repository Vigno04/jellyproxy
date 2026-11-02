package playback

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/config"
)

func TestSessionTrackerLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := cache.NewStorageManager(tempDir, 1.0)
	if err != nil {
		t.Fatalf("Failed to create storage manager: %v", err)
	}

	cfg := &config.Config{
		SharedSecret: "test-secret",
		UpstreamURL:  "http://upstream.local:8096",
	}

	tracker := NewSessionTracker(cfg, storage, nil)

	// 1. Send Playback Start report
	startRep := PlaybackReport{
		Secret:        "test-secret",
		SessionID:     "sess-001",
		DeviceName:    "Living Room TV",
		ClientName:    "AndroidTV",
		UserName:      "Davide",
		ItemID:        "item-123",
		ItemName:      "Inception (2010)",
		PlaySessionID: "play-sess-abc",
		PositionTicks: 0,
		TotalTicks:    10000000000,
		IsPaused:      false,
		EventType:     "start",
		PlayMethod:    "DirectStream",
	}

	body, _ := json.Marshal(startRep)
	req := httptest.NewRequest(http.MethodPost, "/jellyproxy/api/playback/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	tracker.HandleReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HandleReport start returned %d, want 200", w.Code)
	}

	// 2. Ingest an HLS playlist for this session
	playlistContent := `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:3
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-MAP:URI="init.mp4?PlaySessionId=play-sess-abc"
#EXTINF:3.000,
0.m4s?PlaySessionId=play-sess-abc
#EXTINF:3.000,
1.m4s?PlaySessionId=play-sess-abc
#EXTINF:3.000,
2.m4s?PlaySessionId=play-sess-abc
`
	playlistURL := "http://localhost:8080/videos/item-123/hls1/main/main.m3u8?PlaySessionId=play-sess-abc"
	tracker.IngestPlaylist(playlistURL, playlistContent, http.Header{
		"Authorization": []string{"Bearer token-xyz"},
	})

	tracker.mu.RLock()
	sess, ok := tracker.sessions["sess-001"]
	if !ok {
		tracker.mu.RUnlock()
		t.Fatalf("Session sess-001 was not registered")
	}
	if len(sess.Segments) != 4 { // init.mp4 + 3 segments
		tracker.mu.RUnlock()
		t.Fatalf("Expected 4 segments ingested, got %d", len(sess.Segments))
	}
	tracker.mu.RUnlock()

	// 3. Client requests a segment
	segReqURL := "http://localhost:8080/videos/item-123/hls1/main/0.m4s?PlaySessionId=play-sess-abc"
	tracker.OnSegmentRequested(segReqURL, http.Header{})

	tracker.mu.RLock()
	if sess.LastSegmentURL != segReqURL {
		t.Errorf("LastSegmentURL not updated, got %q", sess.LastSegmentURL)
	}
	tracker.mu.RUnlock()

	// 4. Send Pause report
	pauseRep := startRep
	pauseRep.IsPaused = true
	pauseRep.EventType = "pause"
	pauseRep.PositionTicks = 30000000

	body, _ = json.Marshal(pauseRep)
	req = httptest.NewRequest(http.MethodPost, "/jellyproxy/api/playback/report", bytes.NewReader(body))
	w = httptest.NewRecorder()
	tracker.HandleReport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleReport pause returned %d, want 200", w.Code)
	}

	tracker.mu.RLock()
	if !sess.PrefetchActive {
		t.Errorf("Expected PrefetchActive to be true on pause")
	}
	tracker.mu.RUnlock()

	// 5. Send Unpause / Resume report
	unpauseRep := pauseRep
	unpauseRep.IsPaused = false
	unpauseRep.EventType = "unpause"

	body, _ = json.Marshal(unpauseRep)
	req = httptest.NewRequest(http.MethodPost, "/jellyproxy/api/playback/report", bytes.NewReader(body))
	w = httptest.NewRecorder()
	tracker.HandleReport(w, req)

	tracker.mu.RLock()
	if sess.PrefetchActive {
		t.Errorf("Expected PrefetchActive to be false after unpause")
	}
	tracker.mu.RUnlock()

	// 6. Test GET /jellyproxy/api/playback/sessions (Unauthorized without secret)
	reqSessionsUnauth := httptest.NewRequest(http.MethodGet, "/jellyproxy/api/playback/sessions", nil)
	wSessionsUnauth := httptest.NewRecorder()
	tracker.HandleSessions(wSessionsUnauth, reqSessionsUnauth)
	if wSessionsUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("HandleSessions unauthenticated returned %d, want 401", wSessionsUnauth.Code)
	}

	// Authorized with secret
	reqSessions := httptest.NewRequest(http.MethodGet, "/jellyproxy/api/playback/sessions", nil)
	reqSessions.Header.Set("X-JellyProxy-Secret", "test-secret")
	wSessions := httptest.NewRecorder()
	tracker.HandleSessions(wSessions, reqSessions)

	if wSessions.Code != http.StatusOK {
		t.Fatalf("HandleSessions returned %d, want 200", wSessions.Code)
	}

	var list []*SessionInfo
	if err := json.Unmarshal(wSessions.Body.Bytes(), &list); err != nil {
		t.Fatalf("Failed to parse sessions list: %v", err)
	}
	if len(list) != 1 || list[0].SessionID != "sess-001" {
		t.Errorf("Unexpected sessions list: %+v", list)
	}

	// 7. Send Stop report
	stopRep := startRep
	stopRep.EventType = "stop"
	body, _ = json.Marshal(stopRep)
	req = httptest.NewRequest(http.MethodPost, "/jellyproxy/api/playback/report", bytes.NewReader(body))
	w = httptest.NewRecorder()
	tracker.HandleReport(w, req)

	tracker.mu.RLock()
	_, exists := tracker.sessions["sess-001"]
	tracker.mu.RUnlock()
	if exists {
		t.Errorf("Expected session sess-001 to be removed on stop")
	}
}

func reqURL(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}
