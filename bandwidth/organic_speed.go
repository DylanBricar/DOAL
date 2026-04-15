package bandwidth

import (
	"math"
	"math/rand"
	"sync"
)

// OrganicSpeedProvider implements SpeedProvider with a realistic, human-like
// upload speed pattern built from:
//   - random walk with momentum (smooth directional drift)
//   - Gaussian noise (natural variability)
//   - micro-jitter (3% high-frequency noise)
//   - occasional drops (2% chance of brief speed dips)
//
// All values are clamped to [minRate, maxRate].
type OrganicSpeedProvider struct {
	minRate  int64
	maxRate  int64
	current  float64
	momentum float64

	mu sync.Mutex
}

const (
	momentumFactor = 0.7  // how strongly momentum carries between steps
	walkStepFactor = 0.05 // maximum random walk step as fraction of range
	gaussianSigma  = 0.02 // std-dev of Gaussian noise as fraction of range
	microJitter    = 0.03 // micro-jitter amplitude
	dropChance     = 0.02 // probability of a speed drop per tick
	dropFactor     = 0.5  // speed multiplier during a drop
)

// NewOrganicSpeedProvider creates a provider initialised to the midpoint of
// the [minRate, maxRate] range.
func NewOrganicSpeedProvider(minRate, maxRate int64) *OrganicSpeedProvider {
	mid := float64(minRate+maxRate) / 2.0
	return &OrganicSpeedProvider{
		minRate:  minRate,
		maxRate:  maxRate,
		current:  mid,
		momentum: 0,
	}
}

// CurrentSpeed returns the current speed in bytes per second.
func (o *OrganicSpeedProvider) CurrentSpeed() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sample()
}

// Refresh updates the speed model's internal state. This is called
// periodically by the Dispatcher to re-seed long-term drift.
func (o *OrganicSpeedProvider) Refresh() {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Nudge current speed toward a new random target in [min, max].
	rng := float64(o.maxRate-o.minRate)
	target := float64(o.minRate) + rand.Float64()*rng
	o.current = o.current*0.5 + target*0.5
	o.momentum = 0
}

// sample computes one speed observation and advances internal state.
// Caller must hold mu.
func (o *OrganicSpeedProvider) sample() int64 {
	rng := float64(o.maxRate - o.minRate)

	// 1. Random walk step scaled to the range.
	walkStep := (rand.Float64()*2 - 1) * walkStepFactor * rng

	// 2. Apply momentum: blend the walk step with the previous momentum.
	o.momentum = momentumFactor*o.momentum + (1-momentumFactor)*walkStep

	// 3. Advance current speed.
	o.current += o.momentum

	// 4. Gaussian noise.
	gaussianNoise := randGaussian() * gaussianSigma * rng
	o.current += gaussianNoise

	// 5. Micro-jitter.
	jitter := (rand.Float64()*2 - 1) * microJitter * rng
	speed := o.current + jitter

	// 6. Occasional speed drop.
	if rand.Float64() < dropChance {
		speed *= dropFactor
	}

	// 7. Clamp and store the base (without jitter) back into current.
	o.current = clampFloat(o.current, float64(o.minRate), float64(o.maxRate))
	speed = clampFloat(speed, float64(o.minRate), float64(o.maxRate))

	return int64(math.Round(speed))
}

// randGaussian returns a standard normal random value using the
// Box-Muller transform.
func randGaussian() float64 {
	u1 := rand.Float64()
	u2 := rand.Float64()
	// Avoid log(0).
	if u1 == 0 {
		u1 = 1e-10
	}
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
