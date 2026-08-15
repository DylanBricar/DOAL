package bandwidth

import (
	"testing"
	"time"
)

func TestTorrentFlowsHaveIndependentRandomizedTimelines(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	first := newTorrentFlow(now, 100_000, 2_000_000, 11)
	second := newTorrentFlow(now, 100_000, 2_000_000, 29)

	if first.warmupDelay == second.warmupDelay &&
		first.warmupDuration == second.warmupDuration &&
		first.nextRefresh.Equal(second.nextRefresh) {
		t.Fatal("independent torrent RNGs produced an identical timing profile")
	}
	if first.rng == second.rng {
		t.Fatal("torrent flows unexpectedly share one RNG")
	}
}

func TestTorrentFlowTimingRanges(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	for seed := int64(1); seed <= 100; seed++ {
		flow := newTorrentFlow(now, 100_000, 2_000_000, seed)
		if flow.warmupDelay < 0 || flow.warmupDelay > 20*time.Second {
			t.Fatalf("seed %d warmup delay %v outside range", seed, flow.warmupDelay)
		}
		if flow.warmupDuration < 45*time.Second || flow.warmupDuration > 180*time.Second {
			t.Fatalf("seed %d warmup duration %v outside range", seed, flow.warmupDuration)
		}
		refreshAfter := flow.nextRefresh.Sub(now)
		if refreshAfter < 12*time.Minute || refreshAfter > 28*time.Minute {
			t.Fatalf("seed %d refresh delay %v outside range", seed, refreshAfter)
		}
	}
}

func TestTorrentFlowHonorsZeroPlateau(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	flow := newTorrentFlow(now, 100_000, 2_000_000, 7)
	flow.warmupStartedAt = now.Add(-10 * time.Minute)
	flow.zeroUntil = now.Add(30 * time.Second)

	if got := flow.sample(now, true); got != 0 {
		t.Fatalf("speed during zero plateau = %d, want 0", got)
	}
}

func TestHeavyTailedTargetHasLongUpperTail(t *testing.T) {
	t.Parallel()

	const minRate, maxRate = int64(100_000), int64(10_000_000)
	median := heavyTailedTarget(minRate, maxRate, 0)
	upperTail := heavyTailedTarget(minRate, maxRate, 2.5)
	lowerTail := heavyTailedTarget(minRate, maxRate, -2.5)

	if upperTail <= median*3 {
		t.Fatalf("upper-tail target %d is not materially above median %d", upperTail, median)
	}
	if lowerTail >= median {
		t.Fatalf("lower-tail target %d should be below median %d", lowerTail, median)
	}
}

func TestDispatcherRegistersOneFlowPerTorrent(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig(100, 2_000, true, "ORGANIC")
	d := NewDispatcher(cfg, NewOrganicSpeedProvider(100_000, 2_000_000), nil)
	d.RegisterTorrent("first", 1000)
	d.RegisterTorrent("second", 1000)

	d.mu.RLock()
	first := d.flows["first"]
	second := d.flows["second"]
	d.mu.RUnlock()
	if first == nil || second == nil {
		t.Fatal("each registered torrent should own a flow")
	}
	if first == second || first.rng == second.rng {
		t.Fatal("registered torrents share statistical state")
	}
}
