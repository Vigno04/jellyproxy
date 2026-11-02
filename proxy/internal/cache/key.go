package cache

import (
    "net/url"
    "strings"
)

// BuildMediaKey normalizes a media URL by retaining its path and stable query
// parameters while removing session and device-specific values.
func BuildMediaKey(rawURL string) string {
    parsed, err := url.Parse(rawURL)
    if err != nil {
        return rawURL
    }
    q := parsed.Query()
    for _, key := range []string{"PlaySessionId", "playsessionid", "playSessionId", "liveStreamId", "LiveStreamId", "api_key", "ApiKey", "deviceId", "DeviceId", "device_id"} {
        q.Del(key)
    }
    result := parsed.Path
    if result == "" {
        result = rawURL
    }
    if encoded := q.Encode(); encoded != "" {
        result += "?" + encoded
    }
    return result
}

// BuildKey returns the canonical raw cache key shared by proxy, tracker and preseed.
func BuildKey(category string, rawURL string) string {
    return strings.TrimSpace(category) + ":" + BuildMediaKey(rawURL)
}
