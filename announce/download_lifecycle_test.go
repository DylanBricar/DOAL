package announce

import (
	"testing"
	"time"
)

func TestDownloadLifecycleStartsIncompleteAndCompletesOnce(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	lifecycle := newDownloadLifecycle(1_000, 100, 100, now, 17)
	lifecycle.bytesPerSecond = 1_000
	lifecycle.warmupUntil = now

	downloaded, left, completed := lifecycle.snapshot(now)
	if downloaded != 0 || left != 1_000 || completed {
		t.Fatalf("initial snapshot = (%d, %d, %v), want (0, 1000, false)", downloaded, left, completed)
	}

	downloaded, left, completed = lifecycle.snapshot(now.Add(2 * time.Second))
	if downloaded != 1_000 || left != 0 || !completed {
		t.Fatalf("completion snapshot = (%d, %d, %v), want (1000, 0, true)", downloaded, left, completed)
	}

	_, _, completed = lifecycle.snapshot(now.Add(3 * time.Second))
	if completed {
		t.Fatal("completed transition should be emitted exactly once")
	}
}

func TestDownloadLifecycleNeverReportsImpossibleCounters(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	lifecycle := newDownloadLifecycle(1_024, 10_000, 10_000, now, 3)
	lifecycle.bytesPerSecond = 1_000_000
	lifecycle.warmupUntil = now

	downloaded, left, _ := lifecycle.snapshot(now.Add(time.Hour))
	if downloaded < 0 || downloaded > 1_024 {
		t.Fatalf("downloaded = %d outside [0, 1024]", downloaded)
	}
	if left < 0 || downloaded+left != 1_024 {
		t.Fatalf("downloaded + left = %d + %d, want 1024", downloaded, left)
	}
}

func TestDownloadLifecyclesArePerTorrent(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	first := newDownloadLifecycle(1<<30, 100, 1_000, now, 1)
	second := newDownloadLifecycle(1<<30, 100, 1_000, now, 2)
	if first.rng == second.rng {
		t.Fatal("download lifecycles share an RNG")
	}
	if first.bytesPerSecond == second.bytesPerSecond && first.warmupUntil.Equal(second.warmupUntil) {
		t.Fatal("download lifecycles have identical randomized state")
	}
}
