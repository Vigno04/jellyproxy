package handlers

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
)

func TestClassifyCacheableRequest(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantCat    string
		wantItemID string
		wantOk     bool
	}{
		{
			name:       "Static JS asset",
			path:       "/web/main.jellyfin.bundle.js",
			wantCat:    cache.CategoryStatic,
			wantItemID: "",
			wantOk:     true,
		},
		{
			name:       "Static HTML entrypoint should not cache",
			path:       "/web/index.html",
			wantCat:    "",
			wantItemID: "",
			wantOk:     false,
		},
		{
			name:       "Image poster",
			path:       "/Items/e439d5e7789a4e2aa9e32e45daec9312/Images/Primary",
			wantCat:    cache.CategoryImages,
			wantItemID: "e439d5e7789a4e2aa9e32e45daec9312",
			wantOk:     true,
		},
		{
			name:       "HLS fMP4 video chunk",
			path:       "/videos/abc123def456/hls1/main/0.m4s",
			wantCat:    cache.CategoryMedia,
			wantItemID: "abc123def456",
			wantOk:     true,
		},
		{
			name:       "HLS init fragment",
			path:       "/videos/abc123def456/hls1/main/init.mp4",
			wantCat:    cache.CategoryMedia,
			wantItemID: "abc123def456",
			wantOk:     true,
		},
		{
			name:       "HLS legacy TS chunk",
			path:       "/videos/abc123def456/hls/stream/12.ts",
			wantCat:    cache.CategoryMedia,
			wantItemID: "abc123def456",
			wantOk:     true,
		},
		{
			name:       "Audio HLS segment",
			path:       "/audio/item999/hls1/main/0.aac",
			wantCat:    cache.CategoryMedia,
			wantItemID: "item999",
			wantOk:     true,
		},
		{
			name:       "Trickplay BIF file",
			path:       "/Videos/abc123def456/Trickplay/320/0.bif",
			wantCat:    cache.CategoryMedia,
			wantItemID: "abc123def456",
			wantOk:     true,
		},
		{
			name:       "HLS master playlist should never cache",
			path:       "/videos/abc123def456/master.m3u8",
			wantCat:    "",
			wantItemID: "",
			wantOk:     false,
		},
		{
			name:       "HLS variant playlist should never cache",
			path:       "/videos/abc123def456/hls1/main/0.m3u8",
			wantCat:    "",
			wantItemID: "",
			wantOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCat, gotItemID, gotOk := classifyCacheableRequest(tt.path)
			if gotOk != tt.wantOk {
				t.Fatalf("classifyCacheableRequest(%q) ok = %v, want %v", tt.path, gotOk, tt.wantOk)
			}
			if gotCat != tt.wantCat {
				t.Errorf("classifyCacheableRequest(%q) cat = %q, want %q", tt.path, gotCat, tt.wantCat)
			}
			if gotItemID != tt.wantItemID {
				t.Errorf("classifyCacheableRequest(%q) itemID = %q, want %q", tt.path, gotItemID, tt.wantItemID)
			}
		})
	}
}

func TestBuildCacheKeyMedia(t *testing.T) {
	req1 := &http.Request{
		URL: &url.URL{
			Path:     "/videos/item123/hls1/main/5.m4s",
			RawQuery: "PlaySessionId=session-user-1&api_key=tokenA&videoCodec=h264&maxBitrate=8000000",
		},
	}

	req2 := &http.Request{
		URL: &url.URL{
			Path:     "/videos/item123/hls1/main/5.m4s",
			RawQuery: "PlaySessionId=session-user-2&api_key=tokenB&videoCodec=h264&maxBitrate=8000000",
		},
	}

	key1 := buildCacheKey(req1, cache.CategoryMedia)
	key2 := buildCacheKey(req2, cache.CategoryMedia)

	if key1 != key2 {
		t.Errorf("Media cache keys should match regardless of PlaySessionId/api_key: key1=%q, key2=%q", key1, key2)
	}

	// Verify codec and bitrate are preserved
	if key1 != "media:/videos/item123/hls1/main/5.m4s?maxBitrate=8000000&videoCodec=h264" {
		t.Errorf("Unexpected normalized media cache key: %q", key1)
	}
}
