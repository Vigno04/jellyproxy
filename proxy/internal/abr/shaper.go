// Package abr implements adaptive upload bandwidth allocation for JellyProxy.
//
// The shaper arbitrates one scarce resource: the home upstream link (X bps).
// Its two jobs are:
//
//  1. Quality ceilings - decide the bitrate each playback session may pull, using
//     *work conserving* weighted max-min fairness. If the demand fits inside X
//     nobody is degraded (spec case 1); when it does not, the surplus is taken
//     from the greedy streams first (spec cases 2, 3, 6).
//  2. Stream sharing - sessions that can be served by the *same* upstream byte
//     stream are collapsed into one pipe (spec case 4), and the video codec is
//     negotiated so clients with different codec support converge on a common
//     one instead of fanning out into N separate transcodes (spec cases 5, 7).
//
// Time-domain behaviour (fast-start bursts, seek borrowing, prefetch yielding)
// lives on top of that in PullShares/BackgroundBps.
package abr

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	idleTimeout = time.Minute

	// PolicyFairShare gives every pipe an equal claim on the link, degrading streams
	// only when the sum of demands exceeds capacity.
	PolicyFairShare = "FairShare"
	// PolicyFCFS admits streams in arrival order and starves latecomers instead of
	// degrading the incumbents.
	PolicyFCFS = "FirstComeFirstServed"

	// steadyCatchUp keeps a steady stream at its allocated ceiling without inflating
	// upstream pull above the negotiated bitrate.
	steadyCatchUp = 1.0
)

// Config is the runtime tuning surface of the shaper.
type Config struct {
	Enabled                bool   `json:"enabled"`
	UploadSpeedBps         int64  `json:"uploadSpeedBps"`
	HeadroomPercent        int    `json:"headroomPercent"`
	SingleClientCeilingBps int64  `json:"singleClientCeilingBps"`
	MinBitrateBps          int64  `json:"minBitrateBps"`
	Policy                 string `json:"policy"`
	AllowDegradingExisting bool   `json:"allowDegradingExistingStreams"`

	// FastStartSeconds is how long a cold stream (just started) keeps its borrowed
	// share. Spec case 8: 8-10s of burst to fill 15-20s of buffer.
	FastStartSeconds int `json:"fastStartSeconds"`
	// FastStartSharePercent is the slice of the link reserved for bursting pipes.
	FastStartSharePercent int `json:"fastStartSharePercent"`
	// SeekBorrowSeconds is the shorter burst window granted by an explicit seek
	// (spec case 12).
	SeekBorrowSeconds int `json:"seekBorrowSeconds"`
	// BackgroundFloorPercent caps speculative traffic (pause prefetch, preseed) while
	// playback is live (spec case 11).
	BackgroundFloorPercent int `json:"backgroundFloorPercent"`
}

// PipeStatus is the telemetry view of one upstream byte stream.
type PipeStatus struct {
	ItemID       string    `json:"itemId"`
	Signature    string    `json:"signature"`
	Clients      int       `json:"clients"`
	ClientIDs    []string  `json:"clientIds,omitempty"`
	Codec        string    `json:"codec,omitempty"`
	Shared       bool      `json:"shared"`
	RequestedBps int64     `json:"requestedBps"`
	AllocatedBps int64     `json:"allocatedBps"`
	PullBps      int64     `json:"pullBps"`
	Weight       float64   `json:"weight"`
	Burst        string    `json:"burst,omitempty"`
	Pulling      int       `json:"pulling"`
	BytesPulled  int64     `json:"bytesPulled"`
	LastSeen     time.Time `json:"lastSeen"`
}

// Status is the telemetry snapshot consumed by the dashboard and the plugin.
type Status struct {
	Config        Config       `json:"config"`
	TotalBps      int64        `json:"totalBps"`
	UsedBps       int64        `json:"usedBps"`
	HeadroomBps   int64        `json:"headroomBps"`
	ActivePipes   int          `json:"activePipes"`
	SharedPipes   int          `json:"sharedPipes"`
	SavedBps      int64        `json:"savedBps"`
	BackgroundBps int64        `json:"backgroundBps"`
	Pipes         []PipeStatus `json:"pipes"`
}

type pipe struct {
	key         string
	item        string
	clients     map[string]bool
	codec       string
	requested   int64
	allocated   int64
	pull        int64
	weight      float64
	lastSeen    time.Time
	firstSeen   time.Time
	burstUntil  time.Time
	burstReason string
	pulling     int
	bytesPulled int64
	bucket      *Bucket
}

// BandwidthManager owns the pipe table and the fairness computation.
type BandwidthManager struct {
	mu         sync.Mutex
	cfg        Config
	pipes      map[string]*pipe
	clientPipe map[string]string
	caps       *Capabilities
	bg         *Bucket
	link       *Bucket
	onChange   func()
}

// SetOnChange registers a callback invoked whenever the configuration or codec
// capabilities change, allowing callers to persist settings to disk.
func (m *BandwidthManager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.caps.mu.Lock()
	m.caps.onChange = fn
	m.caps.mu.Unlock()
	m.mu.Unlock()
}

// NewBandwidthManager builds a manager with normalised defaults.
func NewBandwidthManager(c Config) *BandwidthManager {
	norm := normalize(c)
	m := &BandwidthManager{
		cfg:        norm,
		pipes:      make(map[string]*pipe),
		clientPipe: make(map[string]string),
		caps:       &Capabilities{},
		bg:         NewBucket(0, 64*1024),
		link:       NewBucket(0, 64*1024),
	}
	m.link.SetRate(m.capacityLocked())
	return m
}

func normalize(c Config) Config {
	if c.HeadroomPercent < 0 {
		c.HeadroomPercent = 0
	}
	if c.HeadroomPercent > 99 {
		c.HeadroomPercent = 99
	}
	if c.MinBitrateBps <= 0 {
		c.MinBitrateBps = 1500000
	}
	switch {
	case strings.EqualFold(c.Policy, PolicyFCFS):
		c.Policy = PolicyFCFS
	default:
		c.Policy = PolicyFairShare
	}
	if c.FastStartSeconds <= 0 {
		c.FastStartSeconds = 10
	}
	if c.FastStartSharePercent <= 0 {
		c.FastStartSharePercent = 90
	}
	if c.SeekBorrowSeconds <= 0 {
		c.SeekBorrowSeconds = 4
	}
	if c.BackgroundFloorPercent <= 0 {
		c.BackgroundFloorPercent = 5
	}
	return c
}

// Capabilities exposes the home-server codec report store.
func (m *BandwidthManager) Capabilities() *Capabilities { return m.caps }

// Signature describes the exact upstream stream a client needs. Two clients share a
// pipe - one home-upload pull (spec case 4) - only when every field matches: same
// item, same encoded video stream, same audio selection, and no per-client burned-in
// subtitle (spec cases 5 and 10 would otherwise silently break sharing).
type Signature struct {
	ItemID       string
	Codec        string
	SourceID     string
	AudioIndex   string
	SubIndex     string
	SubBurnIn    bool
	DirectClient string
}

// Key renders the signature as a stable pipe id. Item id and codec are normalised so
// "h265"/"hevc" and dashed/undashed GUIDs collapse onto the same pipe.
func (s Signature) Key() string {
	var b strings.Builder
	b.WriteString(normalizeItem(s.ItemID))
	b.WriteString("|")
	b.WriteString(NormalizeCodec(s.Codec))
	b.WriteString("|")
	b.WriteString(strings.ToLower(s.SourceID))
	b.WriteString("|a")
	b.WriteString(s.AudioIndex)
	b.WriteString("|s")
	if s.SubBurnIn {
		b.WriteString("burn:" + s.SubIndex)
	} else {
		b.WriteString("ext:" + s.SubIndex)
	}
	if s.DirectClient != "" {
		b.WriteString("|direct:" + s.DirectClient)
	}
	return b.String()
}

// SharesWith reports whether two signatures can be served by one upstream stream.
func SharesWith(a, b Signature) bool { return a.Key() == b.Key() }

// Request is one playback negotiation.
type Request struct {
	ClientID     string
	ClientName   string
	ItemID       string
	RequestedBps int64
	Codec        string
	// ClientCodecs is what this device declares it can decode.
	ClientCodecs []string
	SourceID     string
	AudioIndex   string
	SubIndex     string
	SubBurnIn    bool
	DirectPlay   bool
}

// Decision is the shaper's answer to a Request.
type Decision struct {
	AllocatedBps int64  `json:"allocatedBps"`
	Codec        string `json:"codec"`
	Signature    string `json:"signature"`
	Shared       bool   `json:"shared"`
	Burst        string `json:"burst,omitempty"`
}

// Allocate is the simple entry point: register clientID against itemID and return the
// bitrate ceiling the shaper allows.
func (m *BandwidthManager) Allocate(clientID, itemID string, requested int64) int64 {
	return m.Negotiate(Request{ClientID: clientID, ItemID: itemID, RequestedBps: requested}).AllocatedBps
}

// Negotiate registers a session and decides both its bitrate ceiling and - when the
// device supplied a codec list - which codec the group should converge on.
//
// Convergence rule: if another client is already pulling this item in a codec the
// newcomer can decode, the newcomer joins that stream rather than requesting its own
// favourite codec, so one transcode serves both. If the incumbent codec is undecodable
// for the newcomer, we pick the most efficient codec the newcomer *and* the server both
// support and accept two pipes for now - the shaper then accounts for both pulls.
func (m *BandwidthManager) Negotiate(req Request) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.negotiateLocked(req)
}

func (m *BandwidthManager) negotiateLocked(req Request) Decision {
	now := time.Now()
	m.cleanup(now)

	clientID := req.ClientID
	if clientID == "" {
		clientID = "anonymous"
	}
	if req.RequestedBps <= 0 {
		req.RequestedBps = 100_000_000
	}

	sig := Signature{
		ItemID:     req.ItemID,
		Codec:      req.Codec,
		SourceID:   req.SourceID,
		AudioIndex: req.AudioIndex,
		SubIndex:   req.SubIndex,
		SubBurnIn:  req.SubBurnIn,
	}
	if req.DirectPlay {
		sig.DirectClient = clientID
	}

	if len(req.ClientCodecs) > 0 {
		accepted := SortByPreference(NormalizeCodecList(req.ClientCodecs))
		encodable := SortByPreference(m.caps.Encodeable(accepted))
		if len(encodable) == 0 {
			encodable = accepted
		}
		if existing := m.pipeForItem(req.ItemID); existing != nil && existing.codec != "" &&
			existing.codec != CodecUnknown && !req.DirectPlay {
			if containsCodec(accepted, existing.codec) {
				sig.Codec = existing.codec // join the warm stream
			} else {
				sig.Codec = encodable[0]
			}
		} else if len(encodable) > 0 {
			sig.Codec = encodable[0]
		}
	}

	key := sig.Key()
	p := m.pipes[key]
	if p == nil {
		p = &pipe{
			key:       key,
			item:      normalizeItem(req.ItemID),
			codec:     NormalizeCodec(sig.Codec),
			clients:   make(map[string]bool),
			firstSeen: now,
			bucket:    NewBucket(0, 64*1024),
		}
		m.pipes[key] = p
		// A brand new stream is a cold start: grant the fast-start burst (spec case 8).
		p.burstUntil = now.Add(time.Duration(m.cfg.FastStartSeconds) * time.Second)
		p.burstReason = "fast-start"
	}

	// Move the client if its needs changed (codec switch, new audio track, burn-in).
	if old := m.clientPipe[clientID]; old != "" && old != key {
		if op := m.pipes[old]; op != nil {
			delete(op.clients, clientID)
			if len(op.clients) == 0 {
				m.dropPipeLocked(op)
			}
		}
	}

	p.clients[clientID] = true
	m.clientPipe[clientID] = key
	if req.RequestedBps > p.requested {
		p.requested = req.RequestedBps
	}
	p.lastSeen = now

	// A second client joining a warm pipe still needs a short burst: it starts at a
	// different offset (spec case 9), so only the live edge is cold for it.
	if len(p.clients) > 1 && p.burstUntil.Before(now) {
		half := m.cfg.FastStartSeconds / 2
		if half < 1 {
			half = 1
		}
		p.burstUntil = now.Add(time.Duration(half) * time.Second)
		p.burstReason = "late-join"
	}

	m.reallocate()
	burst := ""
	if p.burstUntil.After(now) {
		burst = p.burstReason
	}
	return Decision{
		AllocatedBps: p.allocated,
		Codec:        p.codec,
		Signature:    key,
		Shared:       len(p.clients) > 1,
		Burst:        burst,
	}
}

func normalizeItem(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

func containsCodec(list []string, want string) bool {
	w := NormalizeCodec(want)
	for _, c := range list {
		if NormalizeCodec(c) == w {
			return true
		}
	}
	return false
}

func (m *BandwidthManager) pipeForItem(itemID string) *pipe {
	norm := normalizeItem(itemID)
	var best *pipe
	for _, p := range m.pipes {
		if p.item != norm {
			continue
		}
		if best == nil || p.lastSeen.After(best.lastSeen) {
			best = p
		}
	}
	return best
}

func (m *BandwidthManager) pipeForClient(clientID string) *pipe {
	if clientID == "" {
		return nil
	}
	return m.pipes[m.clientPipe[clientID]]
}

// Touch keeps a pipe alive while its client keeps requesting segments, so a long
// session is not garbage collected as idle between requests.
func (m *BandwidthManager) Touch(clientID, itemID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pipeForClient(clientID)
	if p == nil {
		p = m.pipeForItem(itemID)
	}
	if p != nil {
		p.lastSeen = time.Now()
	}
}

// Seek grants a short borrowed burst to a client that scrubbed its timeline (spec
// case 12): its buffer is gone, so it temporarily takes bandwidth from the other
// streams to re-prime playback without a freeze.
func (m *BandwidthManager) Seek(clientID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pipeForClient(clientID)
	if p == nil {
		return false
	}
	p.burstUntil = time.Now().Add(time.Duration(m.cfg.SeekBorrowSeconds) * time.Second)
	p.burstReason = "seek-borrow"
	m.reallocate()
	return true
}

// ClientAllocation reports the bitrate ceiling and codec currently assigned to a
// client, used when filtering master playlists for that specific device.
func (m *BandwidthManager) ClientAllocation(clientID string) (int64, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pipeForClient(clientID)
	if p == nil {
		return 0, ""
	}
	return p.allocated, p.codec
}

// Fetch is a handle on one in-flight upstream download.
type Fetch struct {
	m     *BandwidthManager
	key   string
	Burst *Bucket
	link  *Bucket
}

// BeginFetch registers an in-flight home-upload pull and returns a bucket pacing it at
// the pipe's current throughput share. The share is re-checked on every chunk, so a
// burst expiring mid-download throttles the stream automatically.
func (m *BandwidthManager) BeginFetch(clientID, itemID string) *Fetch {
	return m.BeginFetchMode(clientID, itemID, false)
}

// BeginFetchMode registers an in-flight pull and distinguishes DirectPlay streams from
// segmented HLS streams so DirectPlay never falsely shares an HLS pipe.
func (m *BandwidthManager) BeginFetchMode(clientID, itemID string, isDirect bool) *Fetch {
	m.mu.Lock()
	if clientID == "" {
		clientID = "anonymous"
	}
	p := m.pipeForClient(clientID)
	if p != nil && isDirect && !strings.Contains(p.key, "|direct:") {
		m.negotiateLocked(Request{
			ClientID:     clientID,
			ItemID:       itemID,
			RequestedBps: p.requested,
			Codec:        p.codec,
			DirectPlay:   true,
		})
		p = m.pipeForClient(clientID)
	}
	if p == nil && !isDirect {
		p = m.pipeForItem(itemID)
		if p != nil && strings.Contains(p.key, "|direct:") {
			p = nil
		}
	}
	if p == nil && itemID != "" && itemID != "unknown" {
		m.negotiateLocked(Request{
			ClientID:     clientID,
			ItemID:       itemID,
			RequestedBps: max64(m.capacityLocked(), 20_000_000),
			DirectPlay:   isDirect,
		})
		p = m.pipeForClient(clientID)
	}
	if p == nil {
		m.mu.Unlock()
		return nil
	}
	if p.bucket == nil {
		p.bucket = NewBucket(0, 64*1024)
	}
	p.pulling++
	p.lastSeen = time.Now()
	m.reallocate()
	key := p.key
	bucket := p.bucket
	link := m.link
	m.mu.Unlock()

	return &Fetch{m: m, key: key, Burst: bucket, link: link}
}

// Wait paces n upstream bytes against both the pipe's persistent token bucket and the
// global home-uplink bucket, guaranteeing aggregate throughput never exceeds capacity.
func (f *Fetch) Wait(ctx context.Context, n int) error {
	if f == nil {
		return nil
	}
	if f.Burst != nil {
		if err := f.Burst.Wait(ctx, n); err != nil {
			return err
		}
	}
	if f.link != nil {
		if err := f.link.Wait(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// End releases the in-flight slot.
func (f *Fetch) End() {
	if f == nil {
		return
	}
	f.m.mu.Lock()
	if p := f.m.pipes[f.key]; p != nil && p.pulling > 0 {
		p.pulling--
	}
	f.m.mu.Unlock()
}

// Account records n bytes genuinely taken from the home upstream.
func (f *Fetch) Account(n int) {
	if f == nil {
		return
	}
	f.m.mu.Lock()
	if p := f.m.pipes[f.key]; p != nil {
		p.bytesPulled += int64(n)
	}
	f.m.mu.Unlock()
}

// UpdateRate lets a live fetch observe a changed share or an expired fast-start burst.
func (f *Fetch) UpdateRate() {
	if f == nil {
		return
	}
	f.m.mu.Lock()
	f.m.reallocate()
	f.m.mu.Unlock()
}

// BackgroundBps is the budget for speculative traffic (pause prefetch, preseed jobs)
// while playback is live: spec case 11 keeps it at a negligible slice of X so a client
// pressing Play always faces an uncongested link. With no live playback it is the whole
// link, which is exactly what an idle-night prefetch wants.
func (m *BandwidthManager) BackgroundBps() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backgroundLocked()
}

func (m *BandwidthManager) backgroundLocked() int64 {
	capacity := m.capacityLocked()
	if capacity <= 0 || !m.cfg.Enabled || len(m.pipes) == 0 {
		return capacity
	}
	return capacity * int64(m.cfg.BackgroundFloorPercent) / 100
}

// BackgroundWait paces n bytes of speculative traffic against the current background
// budget. The budget is re-read on every call, so a client pressing Play throttles an
// in-flight prefetch immediately (spec case 11).
func (m *BandwidthManager) BackgroundWait(ctx context.Context, n int) error {
	m.mu.Lock()
	rate := m.backgroundLocked()
	b := m.bg
	m.mu.Unlock()
	b.SetRate(rate)
	return b.Wait(ctx, n)
}

// Release drops a client from its pipe (playback stopped).
func (m *BandwidthManager) Release(clientID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.clientPipe[clientID]
	if !ok {
		return
	}
	if p := m.pipes[key]; p != nil {
		delete(p.clients, clientID)
		p.lastSeen = time.Now()
		if len(p.clients) == 0 {
			m.dropPipeLocked(p)
		}
	}
	delete(m.clientPipe, clientID)
	m.reallocate()
}

func (m *BandwidthManager) dropPipeLocked(p *pipe) {
	delete(m.pipes, p.key)
	for c := range p.clients {
		if m.clientPipe[c] == p.key {
			delete(m.clientPipe, c)
		}
	}
}

func (m *BandwidthManager) cleanup(now time.Time) {
	for _, p := range m.pipes {
		if now.Sub(p.lastSeen) > idleTimeout && p.pulling == 0 {
			m.dropPipeLocked(p)
		}
	}
}

func (m *BandwidthManager) capacityLocked() int64 {
	if m.cfg.UploadSpeedBps <= 0 {
		return 0
	}
	return m.cfg.UploadSpeedBps * int64(100-m.cfg.HeadroomPercent) / 100
}

// reallocate runs work-conserving weighted max-min fairness over the pipe table, then
// derives each pipe's instantaneous link throughput share.
//
// Fairness is evaluated in *quality units* rather than raw bits: a pipe is charged
// weight bits per unit of perceived quality (h264 = 1.00, hevc = 0.65, av1 = 0.55). Two
// clients watching the same item in different codecs therefore land at a similar
// picture (spec case 7) while the more efficient codec spends fewer real Mbps.
func (m *BandwidthManager) reallocate() {
	capacity := m.capacityLocked()
	if !m.cfg.Enabled {
		if m.link != nil {
			m.link.SetRate(0)
		}
		for _, p := range m.pipes {
			p.allocated = p.requested
			p.pull = 0
			p.weight = 1
			if p.bucket != nil {
				p.bucket.SetRate(0)
			}
		}
		return
	}
	if m.link != nil {
		m.link.SetRate(capacity)
	}

	pipes := make([]*pipe, 0, len(m.pipes))
	for _, p := range m.pipes {
		pipes = append(pipes, p)
	}
	sort.Slice(pipes, func(i, j int) bool { return pipes[i].firstSeen.Before(pipes[j].firstSeen) })

	if m.cfg.Policy == PolicyFCFS {
		rest := capacity
		for _, p := range pipes {
			p.weight = 1
			if rest <= 0 {
				p.allocated = 0
				continue
			}
			p.allocated = min64(p.requested, rest)
			rest -= p.allocated
		}
		m.postProcess()
		m.computePullsLocked(capacity, pipes)
		return
	}

	// A single stream on an otherwise idle link may use the whole pipe, optionally
	// capped so one client cannot monopolise a fat uplink (spec case 2).
	if len(pipes) == 1 && m.cfg.SingleClientCeilingBps > 0 && capacity > m.cfg.SingleClientCeilingBps {
		capacity = m.cfg.SingleClientCeilingBps
	}

	type entry struct {
		p       *pipe
		demandQ float64
		weight  float64
	}
	entries := make([]entry, 0, len(pipes))
	for _, p := range pipes {
		w := CodecEfficiency(p.codec)
		if w <= 0 {
			w = 1
		}
		p.weight = w
		entries = append(entries, entry{p: p, demandQ: float64(p.requested) / w, weight: w})
	}

	budget := float64(capacity)
	remaining := append([]entry(nil), entries...)
	for len(remaining) > 0 {
		sort.Slice(remaining, func(i, j int) bool { return remaining[i].demandQ < remaining[j].demandQ })
		cheapest := remaining[0]

		spendable := float64(0)
		for _, e := range remaining {
			spendable += e.weight
		}
		theta := budget / spendable // common quality level if the group split evenly

		if theta >= cheapest.demandQ {
			// The cheapest demand sits below the common level: satisfy it fully and
			// hand its freed budget to the rest (work conserving, spec case 1).
			cheapest.p.allocated = cheapest.p.requested
			budget -= cheapest.demandQ * cheapest.weight // == requested
			remaining = remaining[1:]
			continue
		}

		// The whole remaining group is capped at the common level: every stream is
		// degraded equally in quality units (spec cases 3, 5, 6, 7).
		for _, e := range remaining {
			e.p.allocated = int64(theta * e.weight)
		}
		break
	}

	m.postProcess()
	m.computePullsLocked(m.capacityLocked(), pipes)
}

// computePullsLocked turns quality ceilings into link throughput. Bursting pipes get a
// reserved pool (spec cases 8 and 12); the rest split what is left proportionally to
// their allocation. Bursting is zero-sum against steady pipes only, so a solitary
// playback never throttles itself.
func (m *BandwidthManager) computePullsLocked(capacity int64, pipes []*pipe) {
	if capacity <= 0 {
		for _, p := range pipes {
			p.pull = 0
			if p.bucket != nil {
				p.bucket.SetRate(0)
			}
		}
		return
	}
	now := time.Now()

	var burst, steady []*pipe
	for _, p := range pipes {
		if p.burstUntil.After(now) {
			burst = append(burst, p)
		} else {
			steady = append(steady, p)
		}
	}

	if len(burst) == 0 {
		total := int64(0)
		for _, p := range pipes {
			total += p.allocated
		}
		for _, p := range pipes {
			rate := int64(float64(p.allocated) * steadyCatchUp)
			if total > capacity && total > 0 {
				rate = capacity * p.allocated / total
			}
			p.pull = clampRate(rate, capacity)
			if p.bucket != nil {
				p.bucket.SetRate(p.pull)
			}
		}
		return
	}

	pool := capacity * int64(m.cfg.FastStartSharePercent) / 100
	perBurst := pool / int64(len(burst))
	rest := capacity - perBurst*int64(len(burst))
	if rest < 0 {
		rest = 0
	}

	totalSteady := int64(0)
	for _, p := range steady {
		totalSteady += max64(p.allocated, m.cfg.MinBitrateBps)
	}

	for _, p := range burst {
		p.pull = clampRate(perBurst, capacity)
		if p.bucket != nil {
			p.bucket.SetRate(p.pull)
		}
	}
	for _, p := range steady {
		if rest <= 0 {
			p.pull = clampRate(capacity/20, capacity) // never fully starve a playing stream
			if p.bucket != nil {
				p.bucket.SetRate(p.pull)
			}
			continue
		}
		share := rest * max64(p.allocated, m.cfg.MinBitrateBps) / totalSteady
		p.pull = clampRate(share, capacity)
		if p.bucket != nil {
			p.bucket.SetRate(p.pull)
		}
	}
}

func clampRate(rate, capacity int64) int64 {
	if rate < 0 {
		return 0
	}
	if rate > capacity {
		return capacity
	}
	return rate
}

// postProcess applies the floor policy and clears allocations on idle pipes.
func (m *BandwidthManager) postProcess() {
	for _, p := range m.pipes {
		if p.requested <= 0 {
			p.allocated = 0
			continue
		}
		if p.allocated > p.requested {
			p.allocated = p.requested
		}
		if p.allocated > 0 && p.allocated < m.cfg.MinBitrateBps &&
			p.requested >= m.cfg.MinBitrateBps && m.cfg.AllowDegradingExisting {
			p.allocated = m.cfg.MinBitrateBps
		}
	}
}

// GetStatus snapshots the pipe table for the API and the dashboard.
func (m *BandwidthManager) GetStatus() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanup(time.Now())
	m.reallocate()

	capacity := m.capacityLocked()
	s := Status{
		Config:        m.cfg,
		TotalBps:      capacity,
		HeadroomBps:   m.cfg.UploadSpeedBps - capacity,
		BackgroundBps: m.backgroundLocked(),
	}
	for _, p := range m.pipes {
		s.ActivePipes++
		s.UsedBps += p.allocated
		if len(p.clients) > 1 {
			s.SharedPipes++
			// Bits the home upload does not have to send twice (spec case 4).
			s.SavedBps += p.allocated * int64(len(p.clients)-1)
		}
		ids := make([]string, 0, len(p.clients))
		for c := range p.clients {
			ids = append(ids, c)
		}
		sort.Strings(ids)
		burst := ""
		if p.burstUntil.After(time.Now()) {
			burst = p.burstReason
		}
		s.Pipes = append(s.Pipes, PipeStatus{
			ItemID:       p.item,
			Signature:    p.key,
			Clients:      len(p.clients),
			ClientIDs:    ids,
			Codec:        p.codec,
			Shared:       len(p.clients) > 1,
			RequestedBps: p.requested,
			AllocatedBps: p.allocated,
			PullBps:      p.pull,
			Weight:       p.weight,
			Burst:        burst,
			Pulling:      p.pulling,
			BytesPulled:  p.bytesPulled,
			LastSeen:     p.lastSeen,
		})
	}
	sort.Slice(s.Pipes, func(i, j int) bool { return s.Pipes[i].LastSeen.After(s.Pipes[j].LastSeen) })
	return s
}

// UpdateConfig swaps the tuning parameters and re-runs the allocation.
func (m *BandwidthManager) UpdateConfig(c Config) {
	m.mu.Lock()
	m.cfg = normalize(c)
	m.reallocate()
	cb := m.onChange
	m.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Config reports the active tuning parameters.
func (m *BandwidthManager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}
