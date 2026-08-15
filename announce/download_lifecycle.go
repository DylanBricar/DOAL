package announce

import (
	"math"
	"math/rand"
	"time"
)

// downloadLifecycle models a single torrent's transition from leecher to seed.
// Scheduler serializes access with scheduledTorrent.mu.
type downloadLifecycle struct {
	rng              *rand.Rand
	total            int64
	downloaded       int64
	bytesPerSecond   float64
	lastUpdate       time.Time
	warmupUntil      time.Time
	stalledUntil     time.Time
	completedEmitted bool
}

func newDownloadLifecycle(total, minRate, maxRate int64, now time.Time, seed int64) *downloadLifecycle {
	if minRate <= 0 {
		minRate = 1
	}
	if maxRate < minRate {
		maxRate = minRate
	}
	rng := rand.New(rand.NewSource(seed))
	median := math.Sqrt(float64(minRate) * float64(maxRate))
	rate := median * math.Exp(0.65*rng.NormFloat64())
	if rate < float64(minRate) {
		rate = float64(minRate)
	}
	if rate > float64(maxRate)*4.5 {
		rate = float64(maxRate) * 4.5
	}

	return &downloadLifecycle{
		rng:            rng,
		total:          total,
		bytesPerSecond: rate,
		lastUpdate:     now,
		warmupUntil:    now.Add(time.Duration(rng.Int63n(int64(45*time.Second) + 1))),
	}
}

func (d *downloadLifecycle) snapshot(now time.Time) (downloaded, left int64, completed bool) {
	downloaded, left, completed = d.peek(now)
	if completed {
		d.completedEmitted = true
	}
	return downloaded, left, completed
}

func (d *downloadLifecycle) peek(now time.Time) (downloaded, left int64, completed bool) {
	d.advance(now)
	left = d.total - d.downloaded
	if left < 0 {
		left = 0
	}
	completed = d.total > 0 && left == 0 && !d.completedEmitted
	return d.downloaded, left, completed
}

func (d *downloadLifecycle) markCompletedEmitted() {
	d.completedEmitted = true
}

func (d *downloadLifecycle) advance(now time.Time) {
	if !now.After(d.lastUpdate) || d.downloaded >= d.total {
		return
	}
	from := d.lastUpdate
	d.lastUpdate = now
	if from.Before(d.warmupUntil) {
		from = d.warmupUntil
	}
	if from.Before(d.stalledUntil) {
		from = d.stalledUntil
	}
	if !now.After(from) {
		return
	}

	gained := int64(d.bytesPerSecond * now.Sub(from).Seconds())
	if gained < 0 || gained > d.total-d.downloaded {
		gained = d.total - d.downloaded
	}
	d.downloaded += gained

	if d.downloaded < d.total && d.rng.Float64() < 0.04 {
		d.stalledUntil = now.Add(10*time.Second + time.Duration(d.rng.Int63n(int64(80*time.Second))))
	}
}
