package cache

import (
	"net/url"
	"strings"
)

// BuildMediaKey normalizes a media URL so that different clients (e.g. Firefox Web
// and Android) requesting the same HLS segment hit the exact same cache entry.
func BuildMediaKey(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	normPath := normalizeMediaPath(parsed.Path)
	if normPath == "" {
		normPath = rawURL
	}

	q := parsed.Query()
	for _, key := range []string{
		"PlaySessionId", "playsessionid", "playSessionId",
		"liveStreamId", "LiveStreamId",
		"api_key", "ApiKey",
		"deviceId", "DeviceId", "device_id",
		"Tag", "tag",
		"VideoBitrate", "videoBitrate",
		"AudioBitrate", "audioBitrate",
		"MaxStreamingBitrate", "maxStreamingBitrate",
		"MinSegments", "minSegments",
		"TranscodingMaxAudioChannels", "transcodingMaxAudioChannels",
		"TranscodeReasons", "transcodeReasons",
		"runtimeTicks", "RuntimeTicks",
		"actualSegmentLengthTicks", "ActualSegmentLengthTicks",
		"SegmentContainer", "segmentContainer",
		"BreakOnNonKeyFrames", "breakOnNonKeyFrames",
		"RequireAvc", "requireAvc",
		"EnableAudioVbrEncoding", "enableAudioVbrEncoding",
		"CopyTimestamps", "copyTimestamps",
		"AllowVideoStreamCopy", "allowVideoStreamCopy",
		"AllowAudioStreamCopy", "allowAudioStreamCopy",
		"AudioSampleRate", "audioSampleRate",
		"MaxFramerate", "maxFramerate",
		"MaxWidth", "maxWidth", "MaxHeight", "maxHeight",
	} {
		q.Del(key)
	}
	for key := range q {
		lk := strings.ToLower(key)
		if strings.HasPrefix(lk, "h264-") || strings.HasPrefix(lk, "hevc-") || strings.HasPrefix(lk, "av1-") || strings.HasPrefix(lk, "vp9-") {
			q.Del(key)
		}
	}
	if msID := q.Get("MediaSourceId"); msID != "" {
		q.Set("MediaSourceId", normalizeID(msID))
	}
	if !strings.EqualFold(q.Get("SubtitleMethod"), "Encode") {
		q.Del("SubtitleMethod")
		q.Del("SubtitleStreamIndex")
	}
	if encoded := q.Encode(); encoded != "" {
		return normPath + "?" + encoded
	}
	return normPath
}

// normalizeMediaPath normalizes item GUIDs in /videos/{id}/... or /audio/{id}/...
// so dashed and undashed item IDs map to the same canonical path.
func normalizeMediaPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i := 0; i+1 < len(parts); i++ {
		seg := strings.ToLower(parts[i])
		if seg == "videos" || seg == "audio" || seg == "items" {
			parts[i] = seg
			parts[i+1] = normalizeID(parts[i+1])
		}
	}
	return strings.Join(parts, "/")
}

func normalizeID(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// BuildKey returns the canonical raw cache key shared by proxy, tracker and preseed.
func BuildKey(category string, rawURL string) string {
	cat := strings.TrimSpace(category)
	if cat == CategoryMedia {
		return cat + ":" + BuildMediaKey(rawURL)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return cat + ":" + rawURL
	}
	q := parsed.Query()
	for _, key := range []string{"PlaySessionId", "playsessionid", "playSessionId", "liveStreamId", "LiveStreamId", "api_key", "ApiKey", "deviceId", "DeviceId", "device_id"} {
		q.Del(key)
	}
	res := parsed.Path
	if res == "" {
		res = rawURL
	}
	if enc := q.Encode(); enc != "" {
		res += "?" + enc
	}
	return cat + ":" + res
}
