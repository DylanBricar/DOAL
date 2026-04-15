package bandwidth

import (
	"math/rand"
	"sync/atomic"
)

// RandomSpeedProvider implements SpeedProvider with a simple uniform random
// speed that is re-drawn on each Refresh call. Between refreshes the speed is
// constant, which avoids the organic drift/momentum of OrganicSpeedProvider.
type RandomSpeedProvider struct {
	minRate int64
	maxRate int64
	current int64
}

// NewRandomSpeedProvider creates a provider with an initial speed drawn
// uniformly from [minRate, maxRate].
func NewRandomSpeedProvider(minRate, maxRate int64) *RandomSpeedProvider {
	r := &RandomSpeedProvider{minRate: minRate, maxRate: maxRate}
	r.Refresh()
	return r
}

// CurrentSpeed returns the current speed in bytes per second.
func (r *RandomSpeedProvider) CurrentSpeed() int64 {
	return atomic.LoadInt64(&r.current)
}

// Refresh picks a new random speed uniformly from [minRate, maxRate].
func (r *RandomSpeedProvider) Refresh() {
	if r.maxRate <= r.minRate {
		atomic.StoreInt64(&r.current, r.minRate)
		return
	}
	atomic.StoreInt64(&r.current, r.minRate+rand.Int63n(r.maxRate-r.minRate))
}
