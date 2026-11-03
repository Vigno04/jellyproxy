// Package preseed pulls whole titles into the VPS cache ahead of a watch event
// ("movie night"), using only the bandwidth live playback is not using.
package preseed

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jellyproxy-project/jellyproxy/proxy/internal/abr"
	"github.com/jellyproxy-project/jellyproxy/proxy/internal/cache"
)

// Job tracks one pre-seeding run.
type Job struct {
	ID            string   `json:"id"`
	ItemName      string   `json:"item_name,omitempty"`
	URLs          []string `json:"urls,omitempty"`
	TotalSegments int      `json:"total_segments"`
	Completed     int      `json:"completed"`
	Status        string   `json:"status"`
	Error         string   `json:"error,omitempty"`
}

// Manager runs pre-seeding jobs.
type Manager struct {
	storage  *cache.StorageManager
	upstream string
	host     string
	client   *http.Client
	throttle abr.WaitFunc

	mu      sync.Mutex
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc
}

// NewManager creates a manager pulling from the upstream Jellyfin base URL.
func NewManager(storage *cache.StorageManager, upstream, host string) *Manager {
	return &Manager{
		storage:  storage,
		upstream: strings.TrimRight(upstream, "/"),
		host:     host,
		client:   &http.Client{Timeout: 2 * time.Minute},
		jobs:     map[string]*Job{},
		cancels:  map[string]context.CancelFunc{},
	}
}

// SetThrottle installs the background budget. While a client is playing it shrinks to
// a negligible slice of the home upload so prefetch yields to live traffic (spec case 11).
func (m *Manager) SetThrottle(w abr.WaitFunc) { m.throttle = w }

// Start launches a job without extra headers.
func (m *Manager) Start(id string, urls []string) error {
	return m.StartWithOptions(id, "", urls, nil)
}

// StartWithOptions launches a job; headers (auth) are replayed on every upstream request.
func (m *Manager) StartWithOptions(id, itemName string, urls []string, headers http.Header) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = time.Now().UTC().Format("20060102T150405.000000000")
	}
	if _, ok := m.jobs[id]; ok {
		return fmt.Errorf("job already exists")
	}
	j := &Job{ID: id, ItemName: itemName, URLs: append([]string(nil), urls...), Status: "running"}
	ctx, cancel := context.WithCancel(context.Background())
	m.jobs[id] = j
	m.cancels[id] = cancel
	go m.run(ctx, j, headers)
	return nil
}

func (m *Manager) run(ctx context.Context, j *Job, headers http.Header) {
	defer func() {
		m.mu.Lock()
		if j.Status == "running" {
			j.Status = "completed"
		}
		delete(m.cancels, j.ID)
		m.mu.Unlock()
	}()
	for _, raw := range j.URLs {
		if ctx.Err() != nil {
			return
		}
		var err error
		if strings.HasSuffix(strings.ToLower(strings.Split(raw, "?")[0]), ".m3u8") {
			err = m.processPlaylist(ctx, j, raw, headers)
		} else {
			err = m.download(ctx, j, raw, headers)
		}
		if err != nil {
			m.fail(j, err)
			return
		}
	}
}

func (m *Manager) processPlaylist(ctx context.Context, j *Job, raw string, headers http.Header) error {
	body, err := m.fetch(ctx, raw, headers)
	if err != nil {
		return err
	}
	base, err := url.Parse(m.upstreamURL(raw))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		ref := ""
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			for _, part := range strings.Split(strings.TrimPrefix(line, "#EXT-X-MAP:"), ",") {
				if strings.HasPrefix(part, "URI=") {
					ref = strings.Trim(strings.TrimPrefix(part, "URI="), "\"")
				}
			}
		case line != "" && !strings.HasPrefix(line, "#"):
			ref = line
		}
		if ref == "" {
			continue
		}
		u, err := base.Parse(ref)
		if err != nil {
			continue
		}
		absolute := u.String()
		if seen[absolute] || !isSegment(absolute) {
			continue
		}
		seen[absolute] = true
		m.mu.Lock()
		j.TotalSegments++
		m.mu.Unlock()
		if err := m.download(ctx, j, absolute, headers); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func isSegment(raw string) bool {
	switch strings.ToLower(filepath.Ext(strings.Split(raw, "?")[0])) {
	case ".m4s", ".ts", ".mp4", ".m4a", ".aac":
		return true
	}
	return false
}

func (m *Manager) upstreamURL(raw string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	return m.upstream + "/" + strings.TrimLeft(raw, "/")
}

func (m *Manager) get(ctx context.Context, raw string, h http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.upstreamURL(raw), nil)
	if err != nil {
		return nil, err
	}
	m.apply(req, h)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream returned %s", resp.Status)
	}
	return resp, nil
}

func (m *Manager) fetch(ctx context.Context, raw string, h http.Header) ([]byte, error) {
	resp, err := m.get(ctx, raw, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(abr.PacedReader(ctx, resp.Body, m.throttle))
}

func (m *Manager) download(ctx context.Context, j *Job, raw string, h http.Header) error {
	key := cache.HashKey(cache.BuildKey(cache.CategoryMedia, raw))
	if m.storage.Has(key) {
		m.increment(j)
		return nil
	}
	resp, err := m.get(ctx, raw, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp, err := m.storage.CreateTempFile()
	if err != nil {
		return err
	}
	path := tmp.Name()
	_, err = io.Copy(tmp, abr.PacedReader(ctx, resp.Body, m.throttle))
	tmp.Close()
	if err == nil {
		err = m.storage.CommitTempFile(path, cache.CacheMetadata{
			Key: key, URL: raw, Category: cache.CategoryMedia,
			ContentType: resp.Header.Get("Content-Type"), Headers: resp.Header, CreatedAt: time.Now().UTC(),
		})
	}
	if err != nil {
		_ = m.storage.Delete(key)
		return err
	}
	m.increment(j)
	return nil
}

func (m *Manager) apply(req *http.Request, h http.Header) {
	for k, vals := range h {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if m.host != "" {
		req.Host = m.host
	}
}

func (m *Manager) increment(j *Job) {
	m.mu.Lock()
	j.Completed++
	m.mu.Unlock()
}

func (m *Manager) fail(j *Job, err error) {
	m.mu.Lock()
	j.Status, j.Error = "failed", err.Error()
	m.mu.Unlock()
}

// Jobs returns a snapshot of every job.
func (m *Manager) Jobs() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		c := *j
		c.URLs = append([]string(nil), j.URLs...)
		out = append(out, &c)
	}
	return out
}

// Cancel stops a running job.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cancel := m.cancels[id]
	if cancel == nil {
		return false
	}
	cancel()
	if j := m.jobs[id]; j != nil {
		j.Status = "cancelled"
	}
	return true
}
