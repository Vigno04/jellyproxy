package abr

import (
	"sort"
	"strings"
	"sync"
)

// CodecUnknown marks a stream whose video codec has not been negotiated yet.
const CodecUnknown = "unknown"

// Canonical codec names used throughout the shaper.
const (
	CodecH264 = "h264"
	CodecHEVC = "hevc"
	CodecAV1  = "av1"
	CodecVP9  = "vp9"
)

// efficiency is the relative bit cost of one unit of perceived quality (h264 = 1).
var efficiency = map[string]float64{
	CodecH264: 1.00,
	CodecVP9:  0.70,
	CodecHEVC: 0.65,
	CodecAV1:  0.55,
}

// NormalizeCodec maps the many spellings (h265, x265, avc1, av01...) to a canonical name.
func NormalizeCodec(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case c == "":
		return ""
	case c == "h265" || c == "x265" || strings.HasPrefix(c, "hevc") || strings.HasPrefix(c, "hvc1") || strings.HasPrefix(c, "hev1"):
		return CodecHEVC
	case c == "h264" || c == "x264" || c == "avc" || strings.HasPrefix(c, "avc1"):
		return CodecH264
	case c == "av1" || strings.HasPrefix(c, "av01"):
		return CodecAV1
	case strings.HasPrefix(c, "vp9") || strings.HasPrefix(c, "vp09"):
		return CodecVP9
	}
	return c
}

// CodecEfficiency returns the quality weight of a codec (lower = cheaper per quality unit).
func CodecEfficiency(c string) float64 {
	if w, ok := efficiency[NormalizeCodec(c)]; ok {
		return w
	}
	return 1
}

// NormalizeCodecList canonicalises and de-duplicates a codec list, dropping blanks.
func NormalizeCodecList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		for _, part := range strings.Split(c, ",") {
			n := NormalizeCodec(part)
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// SortByPreference orders codecs from most to least bit-efficient.
func SortByPreference(in []string) []string {
	out := append([]string(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return CodecEfficiency(out[i]) < CodecEfficiency(out[j]) })
	return out
}

// Capabilities stores which video codecs the home server can encode. Until the
// plugin reports them, every codec is assumed encodable.
type Capabilities struct {
	mu       sync.RWMutex
	encoders map[string]bool
	onChange func()
}

// SetEncoders replaces the encoder list reported by the home server.
func (c *Capabilities) SetEncoders(list []string) {
	c.mu.Lock()
	c.encoders = map[string]bool{}
	for _, e := range NormalizeCodecList(list) {
		c.encoders[e] = true
	}
	cb := c.onChange
	c.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Encoders lists the reported encoders (nil when nothing was reported).
func (c *Capabilities) Encoders() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.encoders))
	for e := range c.encoders {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// Encodeable filters accepted down to the codecs the server can produce.
// When no encoder list has been reported yet, only h264 and hevc are assumed
// hardware-encodable by default so we never force av1_nvenc on GPUs without AV1 encode.
func (c *Capabilities) Encodeable(accepted []string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.encoders) == 0 {
		out := make([]string, 0, len(accepted))
		for _, a := range accepted {
			n := NormalizeCodec(a)
			if n == CodecH264 || n == CodecHEVC {
				out = append(out, a)
			}
		}
		if len(out) > 0 {
			return out
		}
		return append([]string(nil), accepted...)
	}
	out := make([]string, 0, len(accepted))
	for _, a := range accepted {
		if c.encoders[NormalizeCodec(a)] {
			out = append(out, a)
		}
	}
	return out
}
