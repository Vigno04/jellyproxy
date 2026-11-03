package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
)

const maxPlaybackInfoBytes = 8 << 20

// textSubtitleFormats can be converted to WebVTT on the fly by Jellyfin, so they never
// need to be burned into the video (spec case 10).
var textSubtitleFormats = map[string]bool{
	"ass": true, "ssa": true, "srt": true, "subrip": true, "vtt": true,
	"webvtt": true, "sub": true, "smi": true, "ttml": true,
}

// itemFromPlaybackPath extracts the item id from /Items/{id}/PlaybackInfo.
func itemFromPlaybackPath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "Items") {
			return parts[i+1]
		}
	}
	return "unknown"
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	}
	return ""
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// videoCodecs lists the video codecs the device declares for HLS transcoding,
// falling back to DirectPlayProfiles only when TranscodingProfiles has none.
func videoCodecs(body map[string]any) []string {
	profile, _ := body["DeviceProfile"].(map[string]any)
	var out []string
	for _, key := range []string{"TranscodingProfiles", "DirectPlayProfiles"} {
		list, _ := profile[key].([]any)
		for _, e := range list {
			p, _ := e.(map[string]any)
			if asString(p["Type"]) == "Video" {
				out = append(out, strings.Split(asString(p["VideoCodec"]), ",")...)
			}
		}
		if norm := abr.NormalizeCodecList(out); len(norm) > 0 {
			return norm
		}
	}
	return nil
}

// restrictTranscodeCodec pins the transcode to the codec the shaper converged on, so
// clients with overlapping support are fed from one encoded stream (spec cases 5, 7).
func restrictTranscodeCodec(body map[string]any, codec string) {
	if codec == "" || codec == abr.CodecUnknown {
		return
	}
	profile, _ := body["DeviceProfile"].(map[string]any)
	list, _ := profile["TranscodingProfiles"].([]any)
	for _, e := range list {
		p, _ := e.(map[string]any)
		if asString(p["Type"]) != "Video" {
			continue
		}
		for _, c := range strings.Split(asString(p["VideoCodec"]), ",") {
			if abr.NormalizeCodec(c) == codec {
				p["VideoCodec"] = strings.TrimSpace(c)
				break
			}
		}
	}
}

// preferTextSubtitleDelivery removes "Encode" (burn-in) methods for text formats and
// advertises WebVTT as an external format, so Jellyfin delivers ASS/SSA/SRT as a
// separate converted track and the video stream stays shareable (spec case 10).
func preferTextSubtitleDelivery(body map[string]any) bool {
	profile, ok := body["DeviceProfile"].(map[string]any)
	if !ok {
		return false
	}
	list, _ := profile["SubtitleProfiles"].([]any)
	kept := make([]any, 0, len(list)+1)
	hasVTT := false
	for _, e := range list {
		p, _ := e.(map[string]any)
		format := strings.ToLower(asString(p["Format"]))
		method := asString(p["Method"])
		if method == "Encode" && textSubtitleFormats[format] {
			continue
		}
		if (format == "vtt" || format == "webvtt") && method == "External" {
			hasVTT = true
		}
		kept = append(kept, e)
	}
	if !hasVTT {
		kept = append(kept, map[string]any{"Format": "vtt", "Method": "External"})
	}
	profile["SubtitleProfiles"] = kept
	return true
}

// burnsInSubtitles reports whether Jellyfin decided to burn subtitles into the video of
// any returned media source. Image subtitles (PGS, VobSub) land here: they cannot be
// converted to text without OCR.
func burnsInSubtitles(resp []byte) bool {
	var v struct {
		MediaSources []struct {
			TranscodingURL string `json:"TranscodingUrl"`
		} `json:"MediaSources"`
	}
	if json.Unmarshal(resp, &v) != nil {
		return false
	}
	for _, ms := range v.MediaSources {
		if strings.Contains(ms.TranscodingURL, "SubtitleMethod=Encode") {
			return true
		}
	}
	return false
}

// servePlaybackInfo negotiates a playback session with the shaper before Jellyfin sees
// it: it caps the bitrate, converges the codec and forces text subtitle delivery.
func (h *ReverseProxyHandler) servePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	item := itemFromPlaybackPath(r.URL.Path)
	clientID, clientName := clientIdentity(r)
	q := r.URL.Query()

	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, maxPlaybackInfoBytes))
		_ = r.Body.Close()
		if json.Unmarshal(raw, &body) != nil {
			body = nil
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	pick := func(key string) string {
		if v := asString(body[key]); v != "" {
			return v
		}
		return q.Get(key)
	}

	requested := asInt64(body["MaxStreamingBitrate"])
	if profile, ok := body["DeviceProfile"].(map[string]any); ok && requested == 0 {
		requested = asInt64(profile["MaxStreamingBitrate"])
	}
	if requested == 0 {
		requested, _ = strconv.ParseInt(q.Get("maxStreamingBitrate"), 10, 64)
	}

	req := abr.Request{
		ClientID: clientID, ClientName: clientName, ItemID: item, RequestedBps: requested,
		ClientCodecs: videoCodecs(body), SourceID: pick("MediaSourceId"),
		AudioIndex: pick("AudioStreamIndex"), SubIndex: pick("SubtitleStreamIndex"),
	}
	dec := h.abr.Negotiate(req)

	h.applyDecision(body, dec)
	preferTextSubtitleDelivery(body)
	q.Set("maxStreamingBitrate", fmt.Sprint(dec.AllocatedBps))
	r.URL.RawQuery = q.Encode()

	out, status, header, err := h.forwardPlaybackInfo(r, body)
	if err != nil {
		log.Printf("[JellyProxy] PlaybackInfo upstream error: %v", err)
		http.Error(w, "JellyProxy: Upstream Jellyfin unreachable", http.StatusBadGateway)
		return
	}

	// Reconcile the pipe signature with the actual delivery mode and parameters
	// Jellyfin selected, and prefer segmented HLS over monolithic DirectPlay when
	// TranscodingUrl is present so segments can be cached and shared across clients.
	if status == http.StatusOK {
		out = h.reconcilePlaybackResponse(out, req)
	}

	for k, vals := range header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection":
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func (h *ReverseProxyHandler) reconcilePlaybackResponse(out []byte, req abr.Request) []byte {
	var doc map[string]any
	if json.Unmarshal(out, &doc) != nil {
		return out
	}
	sources, ok := doc["MediaSources"].([]any)
	if !ok || len(sources) == 0 {
		return out
	}
	shaping := h.abr != nil && h.abr.Config().Enabled
	reconciled := false
	modified := false

	for _, raw := range sources {
		ms, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		trURL := asString(ms["TranscodingUrl"])
		if trURL != "" {
			if shaping {
				if ms["SupportsDirectPlay"] == true || ms["SupportsDirectStream"] == true {
					ms["SupportsDirectPlay"] = false
					ms["SupportsDirectStream"] = false
					modified = true
				}
			}
			if !reconciled {
				if parsed, err := url.Parse(trURL); err == nil {
					vq := parsed.Query()
					if vc := vq.Get("VideoCodec"); vc != "" && !strings.EqualFold(vc, "copy") {
						req.Codec = abr.NormalizeCodec(vc)
						req.ClientCodecs = nil
					}
					if ai := vq.Get("AudioStreamIndex"); ai != "" {
						req.AudioIndex = ai
					}
					if si := vq.Get("SubtitleStreamIndex"); si != "" {
						req.SubIndex = si
					}
					if msID := vq.Get("MediaSourceId"); msID != "" {
						req.SourceID = msID
					}
					if strings.EqualFold(vq.Get("SubtitleMethod"), "Encode") {
						req.SubBurnIn = true
					}
				}
				req.DirectPlay = false
				h.abr.Negotiate(req)
				reconciled = true
			}
		} else if !reconciled && (ms["SupportsDirectPlay"] == true || ms["SupportsDirectStream"] == true) {
			if br := asInt64(ms["Bitrate"]); br > 0 && br < req.RequestedBps {
				req.RequestedBps = br
			}
			req.DirectPlay = true
			h.abr.Negotiate(req)
			reconciled = true
		}
	}

	if !reconciled && burnsInSubtitles(out) {
		req.SubBurnIn = true
		h.abr.Negotiate(req)
	}
	if !modified {
		return out
	}
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return out
	}
	return rewritten
}

func (h *ReverseProxyHandler) applyDecision(body map[string]any, dec abr.Decision) {
	body["MaxStreamingBitrate"] = dec.AllocatedBps
	if profile, ok := body["DeviceProfile"].(map[string]any); ok {
		profile["MaxStreamingBitrate"] = dec.AllocatedBps
		if h.abr != nil && h.abr.Config().Enabled {
			body["EnableDirectPlay"] = false
		}
	}
	restrictTranscodeCodec(body, dec.Codec)
}

func (h *ReverseProxyHandler) forwardPlaybackInfo(r *http.Request, body map[string]any) ([]byte, int, http.Header, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, nil, err
	}
	out, err := h.buildUpstreamRequest(r)
	if err != nil {
		return nil, 0, nil, err
	}
	out.Body = io.NopCloser(bytes.NewReader(payload))
	out.ContentLength = int64(len(payload))
	out.Header.Del("Content-Length")
	out.Header.Del("Accept-Encoding")
	out.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(out)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaybackInfoBytes))
	return data, resp.StatusCode, resp.Header, err
}

// filterMasterPlaylist drops HLS variants above the client's allocated ceiling, always
// keeping the lowest variant as a floor so playback never has nothing to choose from.
func (h *ReverseProxyHandler) filterMasterPlaylist(r *http.Request, body []byte) []byte {
	clientID, _ := clientIdentity(r)
	ceiling, _ := h.abr.ClientAllocation(clientID)
	return filterVariants(body, ceiling)
}

func filterVariants(body []byte, ceiling int64) []byte {
	if ceiling <= 0 {
		return body
	}
	type variant struct {
		inf, uri string
		bw       int64
	}
	var variants []variant
	var other []string
	minBw := int64(1<<62 - 1)

	lines := strings.Split(string(body), "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF:") {
			other = append(other, lines[i])
			continue
		}
		inf := lines[i]
		var bw int64
		for _, part := range strings.Split(strings.TrimPrefix(trimmed, "#EXT-X-STREAM-INF:"), ",") {
			if strings.HasPrefix(part, "BANDWIDTH=") {
				bw, _ = strconv.ParseInt(strings.TrimPrefix(part, "BANDWIDTH="), 10, 64)
			}
		}
		uri := ""
		if i+1 < len(lines) {
			i++
			uri = lines[i]
		}
		if bw > 0 && bw < minBw {
			minBw = bw
		}
		variants = append(variants, variant{inf, uri, bw})
	}
	if len(variants) == 0 {
		return body
	}

	var kept []string
	for _, v := range variants {
		if v.bw <= ceiling || v.bw == minBw {
			kept = append(kept, v.inf, v.uri)
		}
	}
	var out []string
	for _, l := range other {
		out = append(out, l)
		if strings.HasPrefix(strings.TrimSpace(l), "#EXTM3U") {
			out = append(out, kept...)
		}
	}
	return []byte(strings.Join(out, "\n"))
}
