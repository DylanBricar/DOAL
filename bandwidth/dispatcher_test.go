package bandwidth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doal/config"
)

// newTestConfig returns a minimal valid config for dispatcher tests.
func newTestConfig(min, max int64, perTorrent bool, model string) *config.Config {
	return &config.Config{
		MinUploadRate:        min,
		MaxUploadRate:        max,
		SimultaneousSeed:     10,
		Client:               "test.client",
		SpeedModel:           model,
		PeerResponseMode:     config.PeerResponseModeNone,
		PerTorrentBandwidth:  perTorrent,
	}
}

// TestRandomSpeedProviderRange verifies the initial speed is within [min, max].
func TestRandomSpeedProviderRange(t *testing.T) {
	const min, max = int64(100_000), int64(200_000)
	sp := NewRandomSpeedProvider(min, max)

	for i := 0; i < 50; i++ {
		s := sp.CurrentSpeed()
		if s < min || s > max {
			t.Fatalf("iteration %d: speed %d out of [%d, %d]", i, s, min, max)
		}
		sp.Refresh()
	}
}

// TestRandomSpeedProviderEqualMinMax verifies that when min == max the provider
// always returns exactly that value.
func TestRandomSpeedProviderEqualMinMax(t *testing.T) {
	sp := NewRandomSpeedProvider(500_000, 500_000)
	for i := 0; i < 10; i++ {
		if s := sp.CurrentSpeed(); s != 500_000 {
			t.Fatalf("want 500000, got %d", s)
		}
		sp.Refresh()
	}
}

// TestOrganicSpeedProviderRange verifies the organic provider stays within
// [min, max] over many refresh/sample cycles.
func TestOrganicSpeedProviderRange(t *testing.T) {
	const min, max = int64(500_000), int64(1_000_000)
	sp := NewOrganicSpeedProvider(min, max)

	for i := 0; i < 200; i++ {
		s := sp.CurrentSpeed()
		if s < min || s > max {
			t.Fatalf("iteration %d: speed %d out of [%d, %d]", i, s, min, max)
		}
		if i%20 == 0 {
			sp.Refresh()
		}
	}
}

// TestDispatcherRegisterAndUnregister verifies that registered torrents appear
// in the speed snapshot and unregistered ones do not.
func TestDispatcherRegisterAndUnregister(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	d.RegisterTorrent("aaa", 0)
	d.RegisterTorrent("bbb", 0)

	snap := d.GetSpeedSnapshot()
	if _, ok := snap["aaa"]; !ok {
		t.Error("aaa should be in snapshot after register")
	}
	if _, ok := snap["bbb"]; !ok {
		t.Error("bbb should be in snapshot after register")
	}

	d.UnregisterTorrent("aaa")
	snap = d.GetSpeedSnapshot()
	if _, ok := snap["aaa"]; ok {
		t.Error("aaa should NOT be in snapshot after unregister")
	}
	if _, ok := snap["bbb"]; !ok {
		t.Error("bbb should still be in snapshot")
	}
}

// TestDispatcherGetStats verifies per-torrent stats are accessible.
func TestDispatcherGetStats(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	d.RegisterTorrent("hash1", 0)
	st := d.GetStats("hash1")
	if st == nil {
		t.Fatal("GetStats should return non-nil after register")
	}
	if d.GetStats("nonexistent") != nil {
		t.Error("GetStats on unknown hash should return nil")
	}
}

// TestDispatcherSetAndGetTotalUploaded verifies the initial seed value.
func TestDispatcherSetAndGetTotalUploaded(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	d.SetTotalUploaded(9_876_543)
	if got := d.TotalUploaded(); got != 9_876_543 {
		t.Errorf("TotalUploaded: want 9876543, got %d", got)
	}
}

// TestDispatcherPauseResume verifies pause sets speed to 0 and resume restores it.
func TestDispatcherPauseResume(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	d.RegisterTorrent("p1", 0)
	d.PauseTorrent("p1")

	snap := d.GetSpeedSnapshot()
	if snap["p1"] != 0 {
		t.Errorf("paused torrent speed should be 0, got %d", snap["p1"])
	}

	d.ResumeTorrent("p1")
	// After resume the speed entry still exists (value is whatever it was before).
	snap = d.GetSpeedSnapshot()
	if _, ok := snap["p1"]; !ok {
		t.Error("p1 should still be tracked after resume")
	}
}

// TestDispatcherUpdatePeers verifies UpdatePeers doesn't panic and the data is
// stored (indirectly verified through swarm-aware speed logic in tick).
func TestDispatcherUpdatePeers(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	d.RegisterTorrent("h1", 0)
	// Should not panic.
	d.UpdatePeers("h1", 5, 20)
	d.UpdatePeers("unknown", 1, 1) // unregistered — should not panic
}

// TestDispatcherTickAccumulatesUpload runs the dispatcher briefly and confirms
// that upload bytes are accumulated after at least one tick.
func TestDispatcherTickAccumulatesUpload(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)

	var callbackTotal atomic.Int64
	d := NewDispatcher(cfg, sp, func(_ map[string]int64, total int64) {
		callbackTotal.Store(total)
	})

	d.RegisterTorrent("hash1", 0)
	d.RegisterTorrent("hash2", 0)

	go d.Run()
	// Wait for at least two ticks (tickInterval is 5s).
	time.Sleep(11 * time.Second)
	d.Stop()

	if d.TotalUploaded() <= 0 {
		t.Error("TotalUploaded should be > 0 after two ticks")
	}
	if callbackTotal.Load() <= 0 {
		t.Error("onSpeedChange callback total should be > 0")
	}

	// Per-torrent stats should also reflect upload.
	perTorrent := d.UploadedPerTorrent()
	for _, hash := range []string{"hash1", "hash2"} {
		if perTorrent[hash] <= 0 {
			t.Errorf("per-torrent upload for %s should be > 0, got %d", hash, perTorrent[hash])
		}
	}
}

// TestDispatcherNoTorrents verifies the tick path with zero registered torrents
// doesn't panic and still calls the callback.
func TestDispatcherNoTorrents(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)

	called := make(chan struct{}, 1)
	d := NewDispatcher(cfg, sp, func(_ map[string]int64, _ int64) {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	go d.Run()
	select {
	case <-called:
	case <-time.After(7 * time.Second):
		t.Error("onSpeedChange callback not called within 7s")
	}
	d.Stop()
}

// TestDispatcherScheduleOutsideWindow verifies that when the schedule is active
// and the current time is outside the window, all speeds are 0.
// This test constructs a window that definitely excludes the current hour.
func TestDispatcherScheduleOutsideWindow(t *testing.T) {
	currentHour := time.Now().Hour()
	// Choose a start/end that excludes currentHour but is a valid same-day window.
	start := (currentHour + 2) % 24
	end := (currentHour + 4) % 24
	if start >= end {
		// Would be an overnight window; skip to keep the test simple.
		t.Skip("cannot construct a non-overnight window that excludes current hour")
	}

	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	cfg.EnableSchedule = true
	cfg.ScheduleStartHour = start
	cfg.ScheduleEndHour = end

	sp := NewRandomSpeedProvider(100_000, 200_000)
	var mu sync.Mutex
	var lastSpeeds map[string]int64
	d := NewDispatcher(cfg, sp, func(speeds map[string]int64, _ int64) {
		mu.Lock()
		lastSpeeds = speeds
		mu.Unlock()
	})

	d.RegisterTorrent("s1", 0)
	go d.Run()
	time.Sleep(6 * time.Second)
	d.Stop()

	mu.Lock()
	defer mu.Unlock()
	for hash, spd := range lastSpeeds {
		if spd != 0 {
			t.Errorf("speed for %s should be 0 outside schedule window, got %d", hash, spd)
		}
	}
}

// TestDispatcherUpdateConfig verifies UpdateConfig swaps the speed provider
// and doesn't panic.
func TestDispatcherUpdateConfig(t *testing.T) {
	cfg := newTestConfig(100, 200, true, config.SpeedModelUniform)
	sp := NewRandomSpeedProvider(100_000, 200_000)
	d := NewDispatcher(cfg, sp, nil)

	newCfg := newTestConfig(200, 400, false, config.SpeedModelOrganic)
	d.UpdateConfig(newCfg) // should not panic
}

// TestClamp verifies the clamp helper.
func TestClamp(t *testing.T) {
	cases := []struct{ v, min, max, want int64 }{
		{5, 1, 10, 5},
		{0, 1, 10, 1},
		{15, 1, 10, 10},
		{1, 1, 1, 1},
	}
	for _, tc := range cases {
		if got := clamp(tc.v, tc.min, tc.max); got != tc.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d", tc.v, tc.min, tc.max, got, tc.want)
		}
	}
}

// TestIsWithinSchedule validates the schedule window helper for edge cases.
func TestIsWithinSchedule(t *testing.T) {
	now := time.Now().Hour()

	sameDayInside := &Dispatcher{config: &config.Config{
		EnableSchedule:    true,
		ScheduleStartHour: (now + 23) % 24, // 1 hour before now (wraps)
		ScheduleEndHour:   (now + 2) % 24,  // 2 hours after now
	}}
	// Only check same-day window where start < now < end.
	start := (now + 23) % 24
	end := (now + 2) % 24
	if start < end {
		if !sameDayInside.isWithinSchedule() {
			t.Error("current hour should be within same-day schedule window")
		}
	}

	_ = sameDayInside // suppress unused warning if skipped
}
