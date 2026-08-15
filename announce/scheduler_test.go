package announce

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

func TestTickBoundsConcurrentAnnounces(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	firstRequest := make(chan struct{})
	release := make(chan struct{})
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		select {
		case <-firstRequest:
		default:
			close(firstRequest)
		}
		<-release
		active.Add(-1)
		_, _ = w.Write([]byte("d8:intervali60ee"))
	}))
	defer tracker.Close()

	scheduler := newTestScheduler()
	scheduler.httpClient = tracker.Client()
	for i := 0; i < 40; i++ {
		hash := fmt.Sprintf("%040x", i+1)
		tor := dummyTorrent(fmt.Sprintf("torrent-%d", i), hash)
		tor.AnnounceURLs = []string{tracker.URL}
		scheduler.AddTorrent(tor)
		scheduler.mu.RLock()
		entry := scheduler.announcers[hash]
		scheduler.mu.RUnlock()
		entry.mu.Lock()
		entry.nextAt = time.Now().Add(-time.Second)
		entry.mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		scheduler.tick()
		close(done)
	}()
	<-firstRequest
	time.Sleep(100 * time.Millisecond)
	if got := maximum.Load(); got > 32 {
		close(release)
		<-done
		t.Fatalf("concurrent announces=%d, want <=32", got)
	}
	close(release)
	<-done
}

func TestRemoveWaitsForInFlightAnnounceBeforeStopped(t *testing.T) {
	t.Parallel()

	startedRequest := make(chan struct{})
	releaseStarted := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStarted) }) }
	defer release()
	var eventsMu sync.Mutex
	var events []string
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := r.URL.Query().Get("event")
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
		if event == "started" {
			startOnce.Do(func() { close(startedRequest) })
			<-releaseStarted
		}
		_, _ = w.Write([]byte("d8:intervali60ee"))
	}))
	defer tracker.Close()

	s := newTestScheduler()
	s.httpClient = tracker.Client()
	tor := dummyTorrent("ordered-remove", "ffeeddccbbaa00998877")
	tor.AnnounceURLs = []string{tracker.URL}
	s.AddTorrent(tor)

	announceDone := make(chan struct{})
	go func() {
		s.announceOne(tor.InfoHashHex)
		close(announceDone)
	}()
	<-startedRequest

	removeDone := make(chan struct{})
	go func() {
		s.RemoveTorrent(tor.InfoHashHex)
		close(removeDone)
	}()
	select {
	case <-removeDone:
		t.Fatal("remove completed before the in-flight started announce")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	<-announceDone
	<-removeDone

	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 2 || events[0] != "started" || events[1] != "stopped" {
		t.Fatalf("announce event order = %q, want [started stopped]", events)
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
		PeerID:    "01234567890123456789",
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

func TestSchedulerMatchesSuccessfulUploadWithLabRing(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		_, _ = w.Write([]byte("d8:intervali60e8:completei1e10:incompletei1ee"))
	}))
	defer tracker.Close()

	var uploaded atomic.Int64
	uploaded.Store(10)
	s := newTestScheduler()
	s.httpClient = tracker.Client()
	s.config.EnableLabSybilRing = true
	s.config.LabSybilPeers = 2
	s.getUploaded = func(string) int64 { return uploaded.Load() }

	tor := dummyTorrent("lab-ring", "11223344556677889900")
	tor.Size = 100
	tor.AnnounceURLs = []string{tracker.URL}
	s.AddTorrent(tor)

	uploaded.Store(50)
	s.announceOne(tor.InfoHashHex)

	s.mu.RLock()
	entry := s.announcers[tor.InfoHashHex]
	s.mu.RUnlock()
	if entry == nil || entry.ring == nil {
		t.Fatal("enabled lab ring was not attached to scheduled torrent")
	}
	snapshot := entry.ring.snapshot()
	if snapshot.AccountedUploaded != 50 || snapshot.TotalDownloaded != 40 {
		t.Fatalf("matched snapshot=%+v, want baseline 10 plus delta 40", snapshot)
	}
	if got := requestCount.Load(); got != 5 {
		t.Fatalf("tracker requests=%d, want 1 main plus 4 counterparty announces", got)
	}

	s.RemoveTorrent(tor.InfoHashHex)
	if s.HasTorrent(tor.InfoHashHex) {
		t.Fatal("removed torrent is still scheduled")
	}
	if got := requestCount.Load(); got != 8 {
		t.Fatalf("tracker requests after removal=%d, want main stop plus two counterparty stops", got)
	}
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
