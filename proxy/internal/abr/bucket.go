package abr

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Bucket is a token bucket that paces byte transfers. Its rate is expressed in
// bits per second like every other shaper quantity; a rate <= 0 means unlimited.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

// NewBucket creates a bucket pacing at rateBps with the given burst size in bytes.
func NewBucket(rateBps int64, burstBytes int64) *Bucket {
	b := &Bucket{burst: float64(burstBytes), tokens: float64(burstBytes), last: time.Now()}
	b.rate = float64(rateBps) / 8
	return b
}

// SetRate changes the pacing rate (bits per second).
func (b *Bucket) SetRate(rateBps int64) {
	b.mu.Lock()
	b.refill()
	b.rate = float64(rateBps) / 8
	b.mu.Unlock()
}

func (b *Bucket) refill() {
	now := time.Now()
	if b.rate > 0 {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
}

// Wait blocks until n bytes may be sent, or ctx is cancelled.
func (b *Bucket) Wait(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.rate <= 0 {
		b.mu.Unlock()
		return nil
	}
	b.refill()
	b.tokens -= float64(n)
	var delay time.Duration
	if b.tokens < 0 {
		delay = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SamePolicy reports whether s names a known allocation policy.
func SamePolicy(s string) bool {
	return strings.EqualFold(s, PolicyFairShare) || strings.EqualFold(s, PolicyFCFS)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// WaitFunc blocks until n bytes may be transferred.
type WaitFunc func(ctx context.Context, n int) error

type pacedReader struct {
	ctx  context.Context
	r    io.Reader
	wait WaitFunc
}

// PacedReader wraps r so every read first waits for budget from wait. A nil wait
// returns r unchanged.
func PacedReader(ctx context.Context, r io.Reader, wait WaitFunc) io.Reader {
	if wait == nil {
		return r
	}
	return &pacedReader{ctx: ctx, r: r, wait: wait}
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if len(b) > 32*1024 {
		b = b[:32*1024]
	}
	n, err := p.r.Read(b)
	if n > 0 {
		if werr := p.wait(p.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
