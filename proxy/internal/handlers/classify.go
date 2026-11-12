package handlers

import (
	"net/http"
	"strings"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
)

var staticCacheExtensions = []string{
	".js", ".css", ".woff", ".woff2", ".svg", ".png", ".jpg", ".ico",
}

var mediaSegmentExtensions = []string{
	".m4s", ".ts", ".mp4", ".m4a", ".aac", ".vtt", ".bif",
}

var subtitleExtensions = []string{
	".vtt", ".srt", ".subrip", ".ass", ".ssa", ".ttml", ".sub", ".smi",
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

	// 3. External subtitle streams (/Videos/{id}/{source}/Subtitles/{idx}/.../Stream.subrip)
	if strings.Contains(lowerPath, "/subtitles/") {
		for _, ext := range subtitleExtensions {
			if strings.HasSuffix(lowerPath, ext) {
				return cache.CategoryStatic, extractItemIDFromMediaPath(rawPath), true
			}
		}
	}

	// 4. Media Segments (HLS fMP4 / TS / init.mp4 / subtitles / trickplay)
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

// isWebSocket reports whether req asks for a protocol upgrade.
func isWebSocket(req *http.Request) bool {
	if req == nil {
		return false
	}
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// isDirectStream reports whether a path is a whole-file Jellyfin stream (direct play /
// direct stream / download). These are range-served by upstream and never cached, but
// they do consume home upload, so they are metered and paced.
func isDirectStream(rawPath string) bool {
	p := strings.ToLower(rawPath)
	if strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".mpd") || strings.Contains(p, "/subtitles/") {
		return false
	}
	switch {
	case strings.Contains(p, "/videos/") && (strings.Contains(p, "/stream") || strings.Contains(p, "/original")):
		return true
	case strings.Contains(p, "/audio/") && strings.Contains(p, "/stream"):
		return true
	case strings.Contains(p, "/items/") && strings.HasSuffix(p, "/download"):
		return true
	}
	return false
}
