package abr

import (
	"context"
	"testing"
	"time"
)

const mb = 1_000_000

func newMgr(uploadMbps int64) *BandwidthManager {
	return NewBandwidthManager(Config{Enabled: true, UploadSpeedBps: uploadMbps * mb, MinBitrateBps: 1})
}

// quiet ends the fast-start burst of every pipe so steady-state behaviour can be asserted.
func quiet(m *BandwidthManager) {
	m.mu.Lock()
	for _, p := range m.pipes {
		p.burstUntil = time.Time{}
	}
	m.reallocate()
	m.mu.Unlock()
}

func alloc(m *BandwidthManager, client string) int64 {
	a, _ := m.ClientAllocation(client)
	return a
}

// Case 1: both demands fit inside X, nobody is degraded.
func TestCase1BothFit(t *testing.T) {
	m := newMgr(20)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 2 * mb})
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 10 * mb})
	if alloc(m, "a") != 2*mb || alloc(m, "b") != 10*mb {
		t.Fatalf("got %d / %d", alloc(m, "a"), alloc(m, "b"))
	}
}

// Case 2: a client asking for 5X gets X.
func TestCase2CapsGreedyClient(t *testing.T) {
	m := newMgr(10)
	d := m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 50 * mb})
	if d.AllocatedBps != 10*mb {
		t.Fatalf("got %d, want %d", d.AllocatedBps, 10*mb)
	}
}

// Cases 3 and 6: equal demands split evenly; a greedy client is cut first.
func TestCase3And6Splitting(t *testing.T) {
	m := newMgr(10)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 10 * mb})
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 10 * mb})
	if alloc(m, "a") != 5*mb || alloc(m, "b") != 5*mb {
		t.Fatalf("case 3: got %d / %d", alloc(m, "a"), alloc(m, "b"))
	}

	m = newMgr(10)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 10 * mb})
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 3 * mb})
	if alloc(m, "b") != 3*mb || alloc(m, "a") != 7*mb {
		t.Fatalf("case 6: got a=%d b=%d", alloc(m, "a"), alloc(m, "b"))
	}
}

// Case 4: same title, same stream: one pipe, one pull.
func TestCase4SameMovieSharesPipe(t *testing.T) {
	m := newMgr(10)
	d1 := m.Negotiate(Request{ClientID: "a", ItemID: "aaaa-bbbb", RequestedBps: 4 * mb})
	d2 := m.Negotiate(Request{ClientID: "b", ItemID: "aaaabbbb", RequestedBps: 4 * mb})
	if d1.Signature != d2.Signature || !d2.Shared {
		t.Fatalf("expected shared pipe, got %q vs %q shared=%v", d1.Signature, d2.Signature, d2.Shared)
	}
	st := m.GetStatus()
	if st.ActivePipes != 1 || st.SharedPipes != 1 || st.UsedBps != 4*mb || st.SavedBps != 4*mb {
		t.Fatalf("unexpected status %+v", st)
	}
}

// Case 5: same title, different audio languages cannot share one stream.
func TestCase5DifferentAudioDoesNotShare(t *testing.T) {
	m := newMgr(10)
	d1 := m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 6 * mb, AudioIndex: "1"})
	d2 := m.Negotiate(Request{ClientID: "b", ItemID: "m1", RequestedBps: 6 * mb, AudioIndex: "2"})
	if d1.Signature == d2.Signature {
		t.Fatal("different audio tracks must not share a pipe")
	}
	quiet(m)
	if alloc(m, "a") != 5*mb || alloc(m, "b") != 5*mb {
		t.Fatalf("both should drop to X/2: %d / %d", alloc(m, "a"), alloc(m, "b"))
	}
}

// Case 7: codec convergence and codec-aware quality fairness.
func TestCase7CodecConvergence(t *testing.T) {
	m := newMgr(10)
	d1 := m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 8 * mb, ClientCodecs: []string{"h264", "hevc"}})
	if d1.Codec != CodecHEVC {
		t.Fatalf("most efficient codec should win, got %q", d1.Codec)
	}
	d2 := m.Negotiate(Request{ClientID: "b", ItemID: "m1", RequestedBps: 8 * mb, ClientCodecs: []string{"h264", "hevc"}})
	if d2.Signature != d1.Signature {
		t.Fatal("second client must join the warm hevc stream")
	}

	// Different items, h264-only vs hevc-only: equal quality, fewer real Mbps for hevc.
	m = newMgr(10)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 20 * mb, ClientCodecs: []string{"h264"}})
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 20 * mb, ClientCodecs: []string{"hevc"}})
	a, b := alloc(m, "a"), alloc(m, "b")
	if b >= a {
		t.Fatalf("hevc client should get fewer real bits than h264: a=%d b=%d", a, b)
	}
	if qa, qb := float64(a)/1.0, float64(b)/0.65; qa/qb > 1.01 || qb/qa > 1.01 {
		t.Fatalf("perceived quality should match: %f vs %f", qa, qb)
	}
}

// Case 8: a cold start borrows most of the link; the steady stream keeps a floor.
func TestCase8FastStartBurst(t *testing.T) {
	m := newMgr(10)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 8 * mb})
	quiet(m)
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 8 * mb})
	m.mu.Lock()
	a, b := m.pipes[m.clientPipe["a"]].pull, m.pipes[m.clientPipe["b"]].pull
	m.mu.Unlock()
	if b != 9*mb {
		t.Fatalf("new client should get 90%% of X, got %d", b)
	}
	if a >= b || a <= 0 {
		t.Fatalf("incumbent keeps a small positive share: a=%d b=%d", a, b)
	}
}

// Case 12: a seek re-grants a burst to the seeking client only.
func TestCase12SeekBorrow(t *testing.T) {
	m := newMgr(10)
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 8 * mb})
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 8 * mb})
	quiet(m)
	if !m.Seek("a") {
		t.Fatal("seek should find the client's pipe")
	}
	m.mu.Lock()
	a, b := m.pipes[m.clientPipe["a"]].pull, m.pipes[m.clientPipe["b"]].pull
	m.mu.Unlock()
	if a <= b {
		t.Fatalf("seeking client should borrow bandwidth: a=%d b=%d", a, b)
	}
	if m.Seek("ghost") {
		t.Fatal("unknown client cannot seek")
	}
}

// Case 10: a burned-in subtitle must not share the clean video stream.
func TestCase10BurnInSplitsPipe(t *testing.T) {
	m := newMgr(20)
	d1 := m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 4 * mb, SubIndex: "2"})
	d2 := m.Negotiate(Request{ClientID: "b", ItemID: "m1", RequestedBps: 4 * mb, SubIndex: "2", SubBurnIn: true})
	if d1.Signature == d2.Signature {
		t.Fatal("burn-in client must get its own pipe")
	}
	d3 := m.Negotiate(Request{ClientID: "b", ItemID: "m1", RequestedBps: 4 * mb, SubIndex: "2"})
	if d3.Signature != d1.Signature || !d3.Shared {
		t.Fatal("with external subtitles the clients share again")
	}
}

// Case 11: background traffic yields to live playback and gets everything when idle.
func TestCase11BackgroundYields(t *testing.T) {
	m := newMgr(10)
	if got := m.BackgroundBps(); got != 10*mb {
		t.Fatalf("idle link: background should use it all, got %d", got)
	}
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 8 * mb})
	if got := m.BackgroundBps(); got != 10*mb*5/100 {
		t.Fatalf("playing: background should be 5%% of X, got %d", got)
	}
	m.Release("a")
	if got := m.BackgroundBps(); got != 10*mb {
		t.Fatalf("after stop: background should recover, got %d", got)
	}
}

func TestFCFSStarvesLatecomer(t *testing.T) {
	m := NewBandwidthManager(Config{Enabled: true, UploadSpeedBps: 10 * mb, Policy: PolicyFCFS, MinBitrateBps: 1})
	m.Negotiate(Request{ClientID: "a", ItemID: "m1", RequestedBps: 10 * mb})
	time.Sleep(time.Millisecond)
	m.Negotiate(Request{ClientID: "b", ItemID: "m2", RequestedBps: 5 * mb})
	if alloc(m, "a") != 10*mb || alloc(m, "b") != 0 {
		t.Fatalf("got %d / %d", alloc(m, "a"), alloc(m, "b"))
	}
}

func TestBucketPacesAndCancels(t *testing.T) {
	b := NewBucket(8*1000, 1000) // 1000 bytes/s
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := b.Wait(context.Background(), 500); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("1500 bytes at 1000 B/s with 1000 burst should take ~0.5s, took %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewBucket(8, 1).Wait(ctx, 1000); err == nil {
		t.Fatal("cancelled wait must fail")
	}
	if err := NewBucket(0, 1).Wait(context.Background(), 1<<30); err != nil {
		t.Fatal("rate 0 means unlimited")
	}
}

func TestNormalizeCodec(t *testing.T) {
	for in, want := range map[string]string{"H265": CodecHEVC, "hev1.1.6": CodecHEVC, "avc1.640028": CodecH264, "AV01": CodecAV1, "": ""} {
		if got := NormalizeCodec(in); got != want {
			t.Errorf("NormalizeCodec(%q)=%q want %q", in, got, want)
		}
	}
}

func TestOnTheFlyBitrateSwitchDerivesPlaySessionID(t *testing.T) {
	m := newMgr(18)

	d1 := m.Negotiate(Request{
		ClientID:     "c1",
		ItemID:       "dune",
		RequestedBps: 80 * mb,
		ClientCodecs: []string{"hevc", "h264"},
	})
	if d1.Codec != CodecHEVC || d1.AllocatedBps != 18*mb {
		t.Fatalf("expected c1 to start with hevc @ 18M, got codec=%s alloc=%d", d1.Codec, d1.AllocatedBps)
	}

	f1 := m.BeginFetchMode("c1", "dune", false)
	vb, psid, oldPS, restart := f1.CheckBitrateSwitch(14_360_000, 640_000, "ps1")
	f1.End()
	if restart || vb != 14_360_000 || psid != "ps1" || oldPS != "" {
		t.Fatalf("unexpected initial switch state: vb=%d psid=%s old=%s restart=%v", vb, psid, oldPS, restart)
	}

	// Client 2 joins and only supports h264.
	d2 := m.Negotiate(Request{
		ClientID:     "c2",
		ItemID:       "dune",
		RequestedBps: 8_750_000,
		ClientCodecs: []string{"h264"},
	})
	if d2.Codec != CodecH264 || d2.Shared {
		t.Fatalf("expected c2 to get separate h264 pipe, got codec=%s shared=%v", d2.Codec, d2.Shared)
	}

	f2 := m.BeginFetchMode("c1", "dune", false)
	vb, psid, oldPS, restart = f2.CheckBitrateSwitch(14_360_000, 640_000, "ps1")
	f2.End()
	if !restart || vb != 8_610_000 || psid != "ps1_jp1" || oldPS != "ps1" {
		t.Fatalf("expected downgrade to 8610000 with derived PlaySessionId ps1_jp1, got vb=%d psid=%s old=%s restart=%v",
			vb, psid, oldPS, restart)
	}
	if got := m.ActivePlaySessionID("c1"); got != "ps1_jp1" {
		t.Fatalf("expected ActivePlaySessionID=ps1_jp1, got %q", got)
	}

	// Next segment (7.mp4) keeps the derived PlaySessionId without restarting again.
	f3 := m.BeginFetchMode("c1", "dune", false)
	vb2, psid2, oldPS2, restart2 := f3.CheckBitrateSwitch(14_360_000, 640_000, "ps1")
	f3.End()
	if restart2 || vb2 != 8_610_000 || psid2 != "ps1_jp1" || oldPS2 != "" {
		t.Fatalf("expected steady 8610000 on ps1_jp1, got vb=%d psid=%s old=%s restart=%v",
			vb2, psid2, oldPS2, restart2)
	}

	// Client 2 stops watching; after cooldown, Client 1 auto-upgrades back to 14360000 on ps1_jp2.
	m.Release("c2")
	m.mu.Lock()
	for _, p := range m.pipes {
		p.lastSwitchAt = time.Now().Add(-10 * time.Second)
	}
	m.mu.Unlock()

	f4 := m.BeginFetchMode("c1", "dune", false)
	vb3, psid3, oldPS3, restart3 := f4.CheckBitrateSwitch(14_360_000, 640_000, "ps1")
	f4.End()
	if !restart3 || vb3 != 14_360_000 || psid3 != "ps1_jp2" || oldPS3 != "ps1_jp1" {
		t.Fatalf("expected auto-upgrade to 14360000 on ps1_jp2 killing ps1_jp1, got vb=%d psid=%s old=%s restart=%v",
			vb3, psid3, oldPS3, restart3)
	}
}
