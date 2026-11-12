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

	// Verify real-world Dune URLs from two different devices (one with SubtitleMethod=Encode
	// but no SubtitleStreamIndex, different bitrates, tags, and session IDs) map to the exact same key.
	duneDev1 := &http.Request{
		URL: &url.URL{
			Path:     "/videos/c902ba72-84ba-376a-9778-d9000f04917c/hls1/main/0.ts",
			RawQuery: "DeviceId=TW96aWxsYS81LjA1&MediaSourceId=c902ba7284ba376a9778d9000f04917c&VideoCodec=hevc,h264&AudioCodec=aac&AudioStreamIndex=2&VideoBitrate=7214000&AudioBitrate=1536000&MaxFramerate=23.976025&PlaySessionId=b4445fe5862e4914a51f9be9fd8be704&api_key=fe21aa8082334433818af2acfd9f4a86&SubtitleMethod=Encode&TranscodingMaxAudioChannels=8&RequireAvc=false&Tag=8f06c536bb57c7cd14be0b8bb2804691&SegmentContainer=ts&MinSegments=2&BreakOnNonKeyFrames=True&hevc-level=153&hevc-videobitdepth=10&hevc-profile=main10&aac-audiochannels=8&TranscodeReasons=ContainerBitrateExceedsLimit&runtimeTicks=0&actualSegmentLengthTicks=30000000",
		},
	}
	duneDev2 := &http.Request{
		URL: &url.URL{
			Path:     "/videos/c902ba72-84ba-376a-9778-d9000f04917c/hls1/main/0.ts",
			RawQuery: "DeviceId=TW96aWxsYS81LjA2&MediaSourceId=c902ba7284ba376a9778d9000f04917c&VideoCodec=hevc,h264&AudioCodec=aac&AudioStreamIndex=2&VideoBitrate=6656000&AudioBitrate=1536000&MaxFramerate=23.976025&PlaySessionId=775f9550131e409d9d8bbcb90c40bf49&api_key=2e5d06526d274e7e9ff4e95c513269c0&TranscodingMaxAudioChannels=6&RequireAvc=false&Tag=8f06c536bb57c7cd14be0b8bb2804691&SegmentContainer=ts&MinSegments=1&BreakOnNonKeyFrames=True&hevc-level=153&hevc-videobitdepth=10&hevc-profile=main10&aac-audiochannels=6&TranscodeReasons=ContainerBitrateExceedsLimit&runtimeTicks=0&actualSegmentLengthTicks=30000000",
		},
	}
	if k1, k2 := buildCacheKey(duneDev1, cache.CategoryMedia), buildCacheKey(duneDev2, cache.CategoryMedia); k1 != k2 {
		t.Errorf("Dune device 1 and device 2 keys differed:\n  k1=%s\n  k2=%s", k1, k2)
	}
	if isDirectStream("/Videos/c902ba72-84ba-376a-9778-d9000f04917c/c902ba7284ba376a9778d9000f04917c/Subtitles/7/0/Stream.subrip") {
		t.Error("Subtitle stream must not be classified as DirectStream")
	}
}
