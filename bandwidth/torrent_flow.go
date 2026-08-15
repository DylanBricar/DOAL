package bandwidth

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"hash/fnv"
	"math"
	"math/rand"
	"time"
)

const (
	flowRefreshMin = 12 * time.Minute
	flowRefreshMax = 28 * time.Minute
	warmupDelayMax = 20 * time.Second
	warmupMin      = 45 * time.Second
	warmupMax      = 180 * time.Second
	zeroChance     = 0.012
	zeroMin        = 10 * time.Second
	zeroMax        = 75 * time.Second
)

// torrentFlow owns all stochastic state for one torrent. Callers serialize
// access through Dispatcher.mu, so math/rand.Rand is safe here.
type torrentFlow struct {
	rng             *rand.Rand
	minRate         int64
	maxRate         int64
	current         float64
	target          float64
	momentum        float64
	burstMultiplier float64
	warmupStartedAt time.Time
	warmupDelay     time.Duration
	warmupDuration  time.Duration
	nextRefresh     time.Time
	zeroUntil       time.Time
}

func newTorrentFlow(now time.Time, minRate, maxRate, seed int64) *torrentFlow {
	rng := rand.New(rand.NewSource(seed))
	flow := &torrentFlow{
		rng:             rng,
		minRate:         minRate,
		maxRate:         maxRate,
		burstMultiplier: 1,
		warmupStartedAt: now,
		warmupDelay:     randomDuration(rng, 0, warmupDelayMax),
		warmupDuration:  randomDuration(rng, warmupMin, warmupMax),
		nextRefresh:     now.Add(randomDuration(rng, flowRefreshMin, flowRefreshMax)),
	}
	flow.target = float64(heavyTailedTarget(minRate, maxRate, rng.NormFloat64()))
	flow.current = flow.target
	return flow
}

func (f *torrentFlow) sample(now time.Time, bursts bool) int64 {
	warmupAt := f.warmupStartedAt.Add(f.warmupDelay)
	if now.Before(warmupAt) || now.Before(f.zeroUntil) {
		return 0
	}

	if !now.Before(f.nextRefresh) {
		f.target = float64(heavyTailedTarget(f.minRate, f.maxRate, f.rng.NormFloat64()))
		f.nextRefresh = now.Add(randomDuration(f.rng, flowRefreshMin, flowRefreshMax))
	}

	if f.rng.Float64() < zeroChance {
		f.zeroUntil = now.Add(randomDuration(f.rng, zeroMin, zeroMax))
		return 0
	}

	rangeSize := float64(f.maxRate - f.minRate)
	step := (f.target-f.current)*0.08 + f.rng.NormFloat64()*rangeSize*0.018
	f.momentum = f.momentum*0.72 + step*0.28
	f.current = clampFloat(f.current+f.momentum, float64(f.minRate), float64(f.maxRate))

	f.updateBurst(bursts)
	speed := f.current * f.burstMultiplier
	if speed > float64(f.maxRate) {
		speed = float64(f.maxRate)
	}

	elapsed := now.Sub(warmupAt)
	if elapsed < f.warmupDuration {
		speed *= float64(elapsed) / float64(f.warmupDuration)
	}
	if speed < 0 {
		return 0
	}
	return int64(math.Round(speed))
}

func (f *torrentFlow) updateBurst(enabled bool) {
	if !enabled {
		f.burstMultiplier = 1
		return
	}
	if f.burstMultiplier > burstMinActive {
		f.burstMultiplier *= burstDecayRate
		if f.burstMultiplier <= burstMinActive {
			f.burstMultiplier = 1
		}
		return
	}
	if f.rng.Float64() < burstChance {
		f.burstMultiplier = burstMinMult + f.rng.Float64()*(burstMaxMult-burstMinMult)
	}
}

func (f *torrentFlow) updateRange(minRate, maxRate int64) {
	f.minRate = minRate
	f.maxRate = maxRate
	f.current = clampFloat(f.current, float64(minRate), float64(maxRate))
	f.target = clampFloat(f.target, float64(minRate), float64(maxRate))
}

// heavyTailedTarget maps a standard-normal observation to a log-normal rate.
// The geometric mean is the median, leaving substantially more mass in the
// upper tail than a uniform or Gaussian rate model.
func heavyTailedTarget(minRate, maxRate int64, standardNormal float64) int64 {
	if maxRate <= minRate {
		return minRate
	}
	if minRate <= 0 {
		minRate = 1
	}
	median := math.Sqrt(float64(minRate) * float64(maxRate))
	target := median * math.Exp(0.8*standardNormal)
	return int64(math.Round(clampFloat(target, float64(minRate), float64(maxRate))))
}

func randomDuration(rng *rand.Rand, minDuration, maxDuration time.Duration) time.Duration {
	if maxDuration <= minDuration {
		return minDuration
	}
	return minDuration + time.Duration(rng.Int63n(int64(maxDuration-minDuration)+1))
}

func flowSeed(infoHashHex string) int64 {
	var raw [8]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(raw[:]))
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(infoHashHex))
	return int64(h.Sum64() ^ uint64(time.Now().UnixNano()))
}
