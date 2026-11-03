package handlers

import (
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// authField matches key="value" pairs of the MediaBrowser/Emby authorization header.
var authField = regexp.MustCompile(`(\w+)="([^"]*)"`)

// clientIdentity derives a stable client id and a display name for a request. Jellyfin
// clients identify themselves with a DeviceId (query string or authorization header).
func clientIdentity(r *http.Request) (id, name string) {
	q := r.URL.Query()
	for _, k := range []string{"DeviceId", "deviceId", "device_id"} {
		if v := q.Get(k); v != "" {
			id = v
			break
		}
	}
	var device, client string
	for _, hdr := range []string{"X-Emby-Authorization", "Authorization"} {
		for _, m := range authField.FindAllStringSubmatch(r.Header.Get(hdr), -1) {
			switch strings.ToLower(m[1]) {
			case "deviceid":
				if id == "" {
					id = m[2]
				}
			case "device":
				if device == "" {
					device = m[2]
				}
			case "client":
				if client == "" {
					client = m[2]
				}
			}
		}
	}
	name = device
	if name == "" {
		name = client
	}
	if id == "" {
		id = remoteIP(r)
	}
	if name == "" {
		name = q.Get("DeviceName")
	}
	if unescaped, err := url.QueryUnescape(name); err == nil && unescaped != "" {
		name = unescaped
	}
	return id, name
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// meteredWriter observes every byte written to a client. It also lets the caller
// pace the stream: onWrite may block to enforce the shaper's throughput share.
type meteredWriter struct {
	http.ResponseWriter
	onWrite func(n int)
}

func (m *meteredWriter) Write(p []byte) (int, error) {
	n, err := m.ResponseWriter.Write(p)
	if n > 0 && m.onWrite != nil {
		m.onWrite(n)
	}
	return n, err
}

// Flush lets streaming responses (reverse proxy) push data through the wrapper.
func (m *meteredWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (m *meteredWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }
