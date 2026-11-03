// Package telemetry records, in memory, which bytes the proxy sends to which client
// and pulls from the home server, so the dashboard can draw the live topology.
package telemetry

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const slots = 16 // one-second buckets kept per counter

// counter is a ring of per-second byte totals.
type counter struct {
	secs  [slots]int64
	bytes [slots]int64
	total int64
}

func (c *counter) add(now time.Time, n int64) {
	s := now.Unix()
	i := int(s % slots)
	if c.secs[i] != s {
		c.secs[i], c.bytes[i] = s, 0
	}
	c.bytes[i] += n
	c.total += n
}

// bps averages the last window seconds, in bits per second. The current, still
// incomplete second is skipped so the value does not flicker.
func (c *counter) bps(now time.Time, window int) int64 {
	if window >= slots {
		window = slots - 1
	}
	cur := now.Unix()
	var sum int64
	for i := 0; i < slots; i++ {
		if age := cur - c.secs[i]; age >= 1 && age <= int64(window) {
			sum += c.bytes[i]
		}
	}
	return sum * 8 / int64(window)
}

type clientRec struct {
	id, name, item string
	out, cache     counter
	last           time.Time
}

type itemRec struct {
	pull counter
	last time.Time
}

// Hub accumulates traffic counters.
type Hub struct {
	mu      sync.Mutex
	window  time.Duration
	clients map[string]*clientRec
	items   map[string]*itemRec
	started time.Time
}

// NewHub creates a hub; entries idle for longer than window disappear from snapshots.
func NewHub(window time.Duration) *Hub {
	return &Hub{window: window, clients: map[string]*clientRec{}, items: map[string]*itemRec{}, started: time.Now()}
}

func normItem(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// Sent records n bytes delivered to a client. fromCache distinguishes bytes served
// from the VPS cache (zero home upload) from bytes relayed from the home server.
func (h *Hub) Sent(clientID, clientName, item string, n int, fromCache bool) {
	if h == nil || n <= 0 {
		return
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.clients[clientID]
	if c == nil {
		c = &clientRec{id: clientID}
		h.clients[clientID] = c
	}
	if clientName != "" {
		c.name = clientName
	}
	if item != "" {
		c.item = normItem(item)
	}
	c.last = now
	c.out.add(now, int64(n))
	if fromCache {
		c.cache.add(now, int64(n))
	}
}

// Pulled records n bytes fetched from the home server for an item.
func (h *Hub) Pulled(item string, n int) {
	if h == nil || n <= 0 {
		return
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	key := normItem(item)
	r := h.items[key]
	if r == nil {
		r = &itemRec{}
		h.items[key] = r
	}
	r.last = now
	r.pull.add(now, int64(n))
}

// ClientFlow is one client's current delivery.
type ClientFlow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ItemID      string `json:"itemId"`
	ItemName    string `json:"itemName"`
	OutBps      int64  `json:"outBps"`
	CacheBps    int64  `json:"cacheBps"`
	UpstreamBps int64  `json:"upstreamBps"`
	TotalBytes  int64  `json:"totalBytes"`
}

// ItemFlow is the home-server pull for one title, shared by every client watching it.
type ItemFlow struct {
	ItemID   string `json:"itemId"`
	Name     string `json:"name"`
	PullBps  int64  `json:"pullBps"`
	Clients  int    `json:"clients"`
	OutBps   int64  `json:"outBps"`
	Pulled   int64  `json:"pulledBytes"`
	Shared   bool   `json:"shared"`
	Colorway int    `json:"hue"`
}

// Snapshot is the dashboard's view of live traffic.
type Snapshot struct {
	At          time.Time    `json:"at"`
	UptimeSec   int64        `json:"uptimeSec"`
	Clients     []ClientFlow `json:"clients"`
	Items       []ItemFlow   `json:"items"`
	PullBps     int64        `json:"pullBps"`
	OutBps      int64        `json:"outBps"`
	CacheBps    int64        `json:"cacheBps"`
	TotalOut    int64        `json:"totalOutBytes"`
	TotalPulled int64        `json:"totalPulledBytes"`
}

// Snapshot returns live flows; names resolves item ids to titles.
func (h *Hub) Snapshot(names *ItemNames) Snapshot {
	now := time.Now()
	win := int(h.window / time.Second)
	if win < 1 {
		win = 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	s := Snapshot{At: now, UptimeSec: int64(now.Sub(h.started).Seconds()), Clients: []ClientFlow{}, Items: []ItemFlow{}}
	byItem := map[string]*ItemFlow{}
	for id, c := range h.clients {
		if now.Sub(c.last) > h.window {
			if now.Sub(c.last) > 10*h.window {
				delete(h.clients, id)
			}
			continue
		}
		out, cache := c.out.bps(now, win), c.cache.bps(now, win)
		cf := ClientFlow{ID: id, Name: c.name, ItemID: c.item, ItemName: names.Name(c.item), OutBps: out, CacheBps: cache,
			UpstreamBps: max(0, out-cache), TotalBytes: c.out.total}
		if cf.Name == "" {
			cf.Name = shortID(id)
		}
		s.Clients = append(s.Clients, cf)
		s.OutBps += out
		s.CacheBps += cache
		s.TotalOut += c.out.total
		f := byItem[c.item]
		if f == nil {
			f = &ItemFlow{ItemID: c.item, Name: names.Name(c.item), Colorway: Hue(c.item)}
			byItem[c.item] = f
		}
		f.Clients++
		f.OutBps += out
	}
	for id, r := range h.items {
		if now.Sub(r.last) > h.window {
			if now.Sub(r.last) > 10*h.window {
				delete(h.items, id)
			}
			continue
		}
		f := byItem[id]
		if f == nil {
			f = &ItemFlow{ItemID: id, Name: names.Name(id), Colorway: Hue(id)}
			byItem[id] = f
		}
		f.PullBps = r.pull.bps(now, win)
		f.Pulled = r.pull.total
		s.PullBps += f.PullBps
	}
	for _, r := range h.items {
		s.TotalPulled += r.pull.total
	}
	for _, f := range byItem {
		f.Shared = f.Clients > 1
		s.Items = append(s.Items, *f)
	}
	sort.Slice(s.Clients, func(i, j int) bool { return s.Clients[i].ID < s.Clients[j].ID })
	sort.Slice(s.Items, func(i, j int) bool { return s.Items[i].ItemID < s.Items[j].ItemID })
	return s
}

// Hue maps an item id to a stable colour hue (0-359) so one title is one colour.
func Hue(item string) int {
	var h uint32 = 2166136261
	for _, c := range []byte(normItem(item)) {
		h = (h ^ uint32(c)) * 16777619
	}
	return int(h % 360)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ItemNames maps item ids to human-readable titles (fed by playback reports).
type ItemNames struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewItemNames creates an empty title registry.
func NewItemNames() *ItemNames { return &ItemNames{m: map[string]string{}} }

// Set records a title.
func (n *ItemNames) Set(id, name string) {
	if n == nil || id == "" || name == "" {
		return
	}
	n.mu.Lock()
	n.m[normItem(id)] = name
	n.mu.Unlock()
}

// Name returns the title, or a short id when unknown.
func (n *ItemNames) Name(id string) string {
	if id == "" {
		return "unknown"
	}
	if n != nil {
		n.mu.RLock()
		name, ok := n.m[normItem(id)]
		n.mu.RUnlock()
		if ok {
			return name
		}
	}
	return shortID(normItem(id))
}
