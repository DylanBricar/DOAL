package bandwidth

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"doal/config"
)

const (
	tickInterval         = 5 * time.Second
	speedRefreshInterval = 20 * time.Minute

	burstChance     = 0.05 // 5% chance of burst per tick
	burstMinMult    = 1.5
	burstMaxMult    = 3.0
	burstDecayRate  = 0.85 // multiplier decays each tick
	burstMinActive  = 1.01 // burst considered active above this multiplier

	swarmAwareMultiplier = 1.2 // speed boost when leechers > 2 * seeders
)

// SpeedProvider abstracts the upload speed computation strategy.
type SpeedProvider interface {
	CurrentSpeed() int64
	Refresh()
}

// TorrentStats tracks cumulative upload/download/left for a single torrent.
type TorrentStats struct {
	Uploaded   atomic.Int64
	Downloaded atomic.Int64
	Left       atomic.Int64
}

// Peers holds the last-known seeder/leecher counts from the tracker response.
type Peers struct {
	Seeders  int
	Leechers int
}

// Dispatcher computes per-torrent speeds and accumulates upload statistics.
// It is the central coordinator between the speed model, the scheduler, and
// the torrent announce loop.
type Dispatcher struct {
	config        *config.Config
	speedProvider SpeedProvider
	stats         map[string]*TorrentStats // infoHashHex -> stats
	speeds        map[string]int64         // infoHashHex -> bytes/s
	peers         map[string]Peers         // infoHashHex -> seeders/leechers
	paused        map[string]bool          // infoHashHex -> paused
	torrentSizes  map[string]int64         // infoHashHex -> total size in bytes
	mu            sync.RWMutex
	totalUploaded int64 // accessed exclusively via atomic ops — mu is NOT used for this field
	onSpeedChange func(speeds map[string]int64, totalUploaded int64)
	stop          chan struct{}
	stopOnce      sync.Once

	// burst state, protected by mu
	burstMultiplier float64
	lastRefresh     time.Time

	// startTime tracks when Run() was called, used for speed warmup.
	startTime time.Time
}

// NewDispatcher creates a Dispatcher with the given config and speed provider.
// onSpeedChange is called every tick with the current per-torrent speeds and
// the cumulative total uploaded bytes.
func NewDispatcher(
	cfg *config.Config,
	sp SpeedProvider,
	onSpeedChange func(speeds map[string]int64, totalUploaded int64),
) *Dispatcher {
	return &Dispatcher{
		config:        cfg,
		speedProvider: sp,
		stats:         make(map[string]*TorrentStats),
		speeds:        make(map[string]int64),
		peers:         make(map[string]Peers),
		paused:        make(map[string]bool),
		torrentSizes:  make(map[string]int64),
		onSpeedChange: onSpeedChange,
		stop:          make(chan struct{}),
		lastRefresh:   time.Now(),
	}
}

// SetTotalUploaded sets the seed value for the cumulative upload counter,
// typically loaded from persistent storage at startup.
func (d *Dispatcher) SetTotalUploaded(total int64) {
	atomic.StoreInt64(&d.totalUploaded, total)
}

// TotalUploaded returns the current cumulative uploaded bytes.
func (d *Dispatcher) TotalUploaded() int64 {
	return atomic.LoadInt64(&d.totalUploaded)
}

// UploadedPerTorrent returns per-torrent uploaded bytes.
func (d *Dispatcher) UploadedPerTorrent() map[string]int64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := make(map[string]int64, len(d.stats))
	for k, v := range d.stats {
		result[k] = v.Uploaded.Load()
	}
	return result
}

// RegisterTorrent adds a torrent to the dispatcher. Safe to call after Run.
// size is the total torrent size in bytes, used for ratio enforcement.
func (d *Dispatcher) RegisterTorrent(infoHashHex string, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.stats[infoHashHex]; !exists {
		d.stats[infoHashHex] = &TorrentStats{}
		d.speeds[infoHashHex] = 0
		d.peers[infoHashHex] = Peers{}
		d.torrentSizes[infoHashHex] = size
	}
}

// UnregisterTorrent removes a torrent from the dispatcher.
func (d *Dispatcher) UnregisterTorrent(infoHashHex string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.stats, infoHashHex)
	delete(d.speeds, infoHashHex)
	delete(d.peers, infoHashHex)
	delete(d.torrentSizes, infoHashHex)
}

// UpdatePeers records the latest seeder/leecher counts for a torrent.
func (d *Dispatcher) UpdatePeers(infoHashHex string, seeders, leechers int) {
	d.mu.Lock()
	d.peers[infoHashHex] = Peers{Seeders: seeders, Leechers: leechers}
	d.mu.Unlock()
}

// GetStats returns the TorrentStats for a given torrent, or nil if unknown.
func (d *Dispatcher) GetStats(infoHashHex string) *TorrentStats {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.stats[infoHashHex]
}

// Run starts the main dispatch loop. It blocks until Stop is called.
func (d *Dispatcher) Run() {
	d.startTime = time.Now()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.tick()
		}
	}
}

// Stop signals the Run loop to exit. Safe to call multiple times.
func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

// tick performs one dispatch cycle: compute speeds, accumulate uploads,
// and notify the callback.
func (d *Dispatcher) tick() {
	d.mu.Lock()

	// Refresh base speed every 20 minutes.
	if time.Since(d.lastRefresh) >= speedRefreshInterval {
		d.speedProvider.Refresh()
		d.lastRefresh = time.Now()
	}

	// If schedule is active and we are outside the allowed window, speeds are 0.
	if d.config.EnableSchedule && !d.isWithinSchedule() {
		for k := range d.speeds {
			d.speeds[k] = 0
		}
		speeds := d.snapshotSpeeds()
		total := atomic.LoadInt64(&d.totalUploaded)
		d.mu.Unlock()
		if d.onSpeedChange != nil {
			d.onSpeedChange(speeds, total)
		}
		return
	}

	baseSpeed := d.speedProvider.CurrentSpeed()
	d.updateBurst()

	torrentCount := len(d.stats)
	if torrentCount == 0 {
		speeds := d.snapshotSpeeds()
		total := atomic.LoadInt64(&d.totalUploaded)
		d.mu.Unlock()
		if d.onSpeedChange != nil {
			d.onSpeedChange(speeds, total)
		}
		return
	}

	tickSeconds := int64(tickInterval.Seconds())

	for hash := range d.stats {
		speed := d.computeTorrentSpeed(hash, baseSpeed, torrentCount)
		d.speeds[hash] = speed
		if !d.paused[hash] {
			gained := speed * tickSeconds
			if speed > 0 {
				gained += int64(rand.Intn(201)) - 100
			}
			if gained < 0 {
				gained = 0
			}
			d.stats[hash].Uploaded.Add(gained)
			atomic.AddInt64(&d.totalUploaded, gained)
		}
	}

	// Enforce upload ratio target: pause torrents that have reached their ratio.
	if d.config.UploadRatioTarget > 0 {
		for hash, stat := range d.stats {
			if d.paused[hash] {
				continue
			}
			size := d.torrentSizes[hash]
			if size > 0 {
				uploaded := stat.Uploaded.Load()
				ratio := float64(uploaded) / float64(size)
				if ratio >= d.config.UploadRatioTarget {
					d.paused[hash] = true
					d.speeds[hash] = 0
				}
			}
		}
	}

	// Snapshot BEFORE releasing the lock, then call callback AFTER releasing.
	speeds := d.snapshotSpeeds()
	total := atomic.LoadInt64(&d.totalUploaded)
	d.mu.Unlock()

	if d.onSpeedChange != nil {
		d.onSpeedChange(speeds, total)
	}
}

// computeTorrentSpeed calculates the effective bytes/s for a single torrent.
func (d *Dispatcher) computeTorrentSpeed(hash string, baseSpeed int64, torrentCount int) int64 {
	if d.paused[hash] {
		return 0
	}
	speed := baseSpeed

	// When NOT using perTorrentBandwidth, divide global speed across torrents.
	// When perTorrentBandwidth is true, each torrent gets the full speed independently.
	if !d.config.PerTorrentBandwidth && torrentCount > 1 {
		speed = speed / int64(torrentCount)
	}

	p := d.peers[hash]
	noLeechers := p.Leechers == 0

	// Apply swarm-aware boost: torrents with many leechers get a bonus.
	if d.config.SwarmAwareSpeed && !noLeechers && p.Leechers > p.Seeders*2 {
		speed = int64(float64(speed) * swarmAwareMultiplier)
	}

	// Apply burst multiplier.
	if d.config.EnableBurstSpeed && d.burstMultiplier > burstMinActive {
		speed = int64(float64(speed) * d.burstMultiplier)
	}

	// Use minimum speed floor when there are no leechers (config is in kB/s).
	minNoLeechers := d.config.MinSpeedWhenNoLeechers * 1000
	if noLeechers && speed < minNoLeechers {
		speed = minNoLeechers
	}

	// Clamp to configured range (config is in kB/s, speed is in bytes/s).
	minRate := d.config.MinUploadRate * 1000
	maxRate := d.config.MaxUploadRate * 1000
	if d.config.PerTorrentBandwidth {
		// Each torrent gets full min-max range
		speed = clamp(speed, minRate, maxRate)
	} else {
		// Global speed divided across torrents
		speed = clamp(speed, minRate/int64(torrentCount), maxRate/int64(torrentCount))
	}

	// Warmup: gradually increase speed over first 60 seconds.
	elapsed := time.Since(d.startTime).Seconds()
	if elapsed < 60 {
		warmupFactor := elapsed / 60.0 // 0.0 to 1.0
		speed = int64(float64(speed) * warmupFactor)
		if speed < 1000 {
			speed = 1000 // minimum 1 KB/s during warmup
		}
	}

	return speed
}

// updateBurst rolls the dice for a new burst or decays an existing one.
func (d *Dispatcher) updateBurst() {
	if !d.config.EnableBurstSpeed {
		d.burstMultiplier = 1.0
		return
	}

	if d.burstMultiplier > burstMinActive {
		// Decay existing burst.
		d.burstMultiplier *= burstDecayRate
		if d.burstMultiplier <= burstMinActive {
			d.burstMultiplier = 1.0
		}
		return
	}

	// Roll for a new burst.
	if rand.Float64() < burstChance {
		d.burstMultiplier = burstMinMult + rand.Float64()*(burstMaxMult-burstMinMult)
	}
}

// isWithinSchedule returns true when the current hour falls within the
// configured seeding window. Handles overnight windows (e.g., 22:00–07:00).
func (d *Dispatcher) isWithinSchedule() bool {
	hour := time.Now().Hour()
	start := d.config.ScheduleStartHour
	end := d.config.ScheduleEndHour

	if start <= end {
		// Same-day window, e.g., 08:00–20:00.
		return hour >= start && hour < end
	}
	// Overnight window, e.g., 22:00–07:00.
	return hour >= start || hour < end
}

// GetSpeedSnapshot returns a thread-safe copy of the current per-torrent speeds.
func (d *Dispatcher) GetSpeedSnapshot() map[string]int64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.snapshotSpeeds()
}

// snapshotSpeeds returns a copy of the current speeds map. Caller must hold mu.
func (d *Dispatcher) snapshotSpeeds() map[string]int64 {
	snapshot := make(map[string]int64, len(d.speeds))
	for k, v := range d.speeds {
		snapshot[k] = v
	}
	return snapshot
}

// PauseTorrent sets speed to 0 for the given torrent and stops upload accumulation.
func (d *Dispatcher) PauseTorrent(infoHashHex string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused[infoHashHex] = true
	d.speeds[infoHashHex] = 0
}

// ResumeTorrent resumes normal speed calculation for the given torrent.
func (d *Dispatcher) ResumeTorrent(infoHashHex string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.paused, infoHashHex)
}

// UpdateConfig updates the dispatcher's config and refreshes the speed provider.
func (d *Dispatcher) UpdateConfig(cfg *config.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config = cfg
	if cfg.SpeedModel == config.SpeedModelOrganic {
		d.speedProvider = NewOrganicSpeedProvider(cfg.MinUploadRate*1000, cfg.MaxUploadRate*1000)
	} else {
		d.speedProvider = NewRandomSpeedProvider(cfg.MinUploadRate*1000, cfg.MaxUploadRate*1000)
	}
	d.speedProvider.Refresh()
}

func clamp(v, min, max int64) int64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
