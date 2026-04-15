package announce

import (
	"testing"
	"time"

	"doal/config"
	"doal/torrent"
)

// dummyTorrent builds a minimal Torrent for scheduler tests.
func dummyTorrent(name, hash string) *torrent.Torrent {
	var infoHash [20]byte
	copy(infoHash[:], hash)
	return &torrent.Torrent{
		InfoHash:     infoHash,
		InfoHashHex:  hash,
		Name:         name,
		Size:         1 << 20,
		AnnounceURLs: []string{"http://tracker.example.com/announce"},
	}
}

// dummyConfig returns a minimal config for scheduler construction.
func dummyConfig() *config.Config {
	return &config.Config{
		MinUploadRate:    100,
		MaxUploadRate:    200,
		SimultaneousSeed: 5,
		Client:           "test.client",
		SpeedModel:       config.SpeedModelUniform,
		PeerResponseMode: config.PeerResponseModeNone,
	}
}

// newTestScheduler builds a Scheduler backed by a dummy ClientConfig so that
// no real TLS transport is needed. Because the ClientConfig load requires a
// .client file, we build a minimal one via the exported fields directly.
func newTestScheduler() *Scheduler {
	cc := &ClientConfig{
		PeerID:   "01234567890123456789",
		UserAgent: "TestClient/1.0",
		Query:     "info_hash={infohash}&peer_id={peerid}&port={port}&uploaded={uploaded}&downloaded={downloaded}&left={left}&event={event}&numwant=80&compact=1",
		Numwant:   80,
	}

	return &Scheduler{
		announcers:  make(map[string]*scheduledTorrent),
		jitterPct:   0,
		port:        6881,
		client:      cc,
		config:      dummyConfig(),
		onSuccess:   nil,
		onFailure:   nil,
		getUploaded: func(string) int64 { return 0 },
		stop:        make(chan struct{}),
	}
}

// TestSchedulerAddTorrentHasTorrent verifies AddTorrent registers the hash.
func TestSchedulerAddTorrentHasTorrent(t *testing.T) {
	s := newTestScheduler()
	tor := dummyTorrent("alpha", "aaaaaaaaaaaaaaaaaaa1")
	s.AddTorrent(tor)
	if !s.HasTorrent(tor.InfoHashHex) {
		t.Errorf("HasTorrent: want true after AddTorrent, got false")
	}
}

// TestSchedulerAddTorrentIdempotent verifies double-add does not create two entries.
func TestSchedulerAddTorrentIdempotent(t *testing.T) {
	s := newTestScheduler()
	tor := dummyTorrent("beta", "bbbbbbbbbbbbbbbbbbb1")
	s.AddTorrent(tor)
	s.AddTorrent(tor)
	s.mu.RLock()
	count := len(s.announcers)
	s.mu.RUnlock()
	if count != 1 {
		t.Errorf("expected 1 entry after double-add, got %d", count)
	}
}

// TestSchedulerHasTorrentAbsent verifies HasTorrent returns false for unknown hash.
func TestSchedulerHasTorrentAbsent(t *testing.T) {
	s := newTestScheduler()
	if s.HasTorrent("nonexistent") {
		t.Error("HasTorrent: want false for unknown hash, got true")
	}
}

// TestSchedulerPauseAndIsPaused verifies pause toggles the paused field.
func TestSchedulerPauseAndIsPaused(t *testing.T) {
	s := newTestScheduler()
	tor := dummyTorrent("gamma", "ccccccccccccccccccc1")
	s.AddTorrent(tor)

	if s.IsPaused(tor.InfoHashHex) {
		t.Error("IsPaused: want false before PauseTorrent")
	}

	s.PauseTorrent(tor.InfoHashHex)
	if !s.IsPaused(tor.InfoHashHex) {
		t.Error("IsPaused: want true after PauseTorrent")
	}
}

// TestSchedulerResumeClears verifies ResumeTorrent clears the paused flag.
func TestSchedulerResumeClears(t *testing.T) {
	s := newTestScheduler()
	tor := dummyTorrent("delta", "ddddddddddddddddddd1")
	s.AddTorrent(tor)
	s.PauseTorrent(tor.InfoHashHex)
	s.ResumeTorrent(tor.InfoHashHex)

	if s.IsPaused(tor.InfoHashHex) {
		t.Error("IsPaused: want false after ResumeTorrent")
	}
}

// TestSchedulerResumeSchedulesImmediately verifies nextAt is set to now or earlier.
func TestSchedulerResumeSchedulesImmediately(t *testing.T) {
	s := newTestScheduler()
	tor := dummyTorrent("epsilon", "eeeeeeeeeeeeeeeeeee1")
	s.AddTorrent(tor)
	s.PauseTorrent(tor.InfoHashHex)

	before := time.Now()
	s.ResumeTorrent(tor.InfoHashHex)
	after := time.Now()

	s.mu.RLock()
	entry := s.announcers[tor.InfoHashHex]
	s.mu.RUnlock()

	entry.mu.Lock()
	nextAt := entry.nextAt
	entry.mu.Unlock()

	if nextAt.After(after) {
		t.Errorf("nextAt should be <= %v after resume, got %v", after, nextAt)
	}
	_ = before
}

// TestSchedulerSetPort verifies SetPort changes the value GetPort returns.
func TestSchedulerSetPort(t *testing.T) {
	s := newTestScheduler()
	s.SetPort(12345)
	if got := s.GetPort(); got != 12345 {
		t.Errorf("GetPort: want 12345, got %d", got)
	}
}

// TestSchedulerGetPortDefault verifies the initial port is as configured.
func TestSchedulerGetPortDefault(t *testing.T) {
	s := newTestScheduler()
	if got := s.GetPort(); got != 6881 {
		t.Errorf("GetPort: want 6881, got %d", got)
	}
}

// TestSchedulerStopIsIdempotent verifies Stop can be called multiple times safely.
func TestSchedulerStopIsIdempotent(t *testing.T) {
	s := newTestScheduler()
	s.Stop()
	s.Stop() // should not panic
}

// TestSchedulerPauseUnknownHash verifies PauseTorrent on unknown hash does not panic.
func TestSchedulerPauseUnknownHash(t *testing.T) {
	s := newTestScheduler()
	s.PauseTorrent("unknown-hash") // should not panic
}

// TestSchedulerResumeUnknownHash verifies ResumeTorrent on unknown hash does not panic.
func TestSchedulerResumeUnknownHash(t *testing.T) {
	s := newTestScheduler()
	s.ResumeTorrent("unknown-hash") // should not panic
}

// TestSchedulerIsPausedUnknown verifies IsPaused returns false for unregistered hash.
func TestSchedulerIsPausedUnknown(t *testing.T) {
	s := newTestScheduler()
	if s.IsPaused("not-there") {
		t.Error("IsPaused: want false for unknown hash")
	}
}

// TestApplyJitterNoJitter verifies applyJitter with 0% jitter returns exact seconds.
func TestApplyJitterNoJitter(t *testing.T) {
	d := applyJitter(1800, 0)
	if d != 1800*time.Second {
		t.Errorf("applyJitter(1800, 0): want 1800s, got %v", d)
	}
}

// TestApplyJitterZeroInterval verifies applyJitter with 0-second interval returns 0.
func TestApplyJitterZeroInterval(t *testing.T) {
	d := applyJitter(0, 10)
	if d != 0 {
		t.Errorf("applyJitter(0, 10): want 0, got %v", d)
	}
}

// TestApplyJitterWithinBounds verifies applyJitter stays within ±jitterPct% of interval.
func TestApplyJitterWithinBounds(t *testing.T) {
	interval := 1800
	jitter := 20
	min := time.Duration(float64(interval)*(1.0-float64(jitter)/100.0)) * time.Second
	max := time.Duration(float64(interval)*(1.0+float64(jitter)/100.0)) * time.Second

	for i := 0; i < 100; i++ {
		d := applyJitter(interval, jitter)
		if d < min || d > max {
			t.Errorf("iteration %d: applyJitter(%d, %d) = %v outside [%v, %v]",
				i, interval, jitter, d, min, max)
		}
	}
}

// TestSchedulerMultipleTorrents verifies multiple torrents can coexist.
func TestSchedulerMultipleTorrents(t *testing.T) {
	s := newTestScheduler()
	hashes := []string{
		"hash1111111111111111",
		"hash2222222222222222",
		"hash3333333333333333",
	}
	for _, h := range hashes {
		s.AddTorrent(dummyTorrent(h, h))
	}
	for _, h := range hashes {
		if !s.HasTorrent(h) {
			t.Errorf("HasTorrent(%q): want true", h)
		}
	}
}
