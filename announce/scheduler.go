package announce

import (
	"context"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"doal/config"
	"doal/torrent"
)

// scheduledTorrent tracks an Announcer alongside its next scheduled announce time.
type scheduledTorrent struct {
	mu               sync.Mutex
	announcer        *Announcer
	nextAt           time.Time
	started          bool
	paused           bool
	downloadedSoFar  int64
	completedSent    bool
	consecutiveFails int
	announcing       bool // prevents double-announce when tick fires before previous finishes
}

// Scheduler manages periodic announces for a set of torrents.
// It honours per-torrent intervals with configurable jitter.
type Scheduler struct {
	announcers    map[string]*scheduledTorrent // infoHashHex -> entry
	mu            sync.RWMutex
	jitterPct     int
	port          int
	portMu        sync.RWMutex
	client        *ClientConfig
	config        *config.Config
	httpClient    *http.Client
	onSuccess      func(infoHashHex string, resp *AnnounceResponse)
	onFailure      func(infoHashHex string, err error)
	onTooManyFails func(infoHashHex string)
	getUploaded    func(infoHashHex string) int64 // fetch uploaded bytes from dispatcher
	stop           chan struct{}
	stopOnce       sync.Once
}

// NewScheduler constructs a Scheduler with the given settings.
// onSuccess and onFailure callbacks are invoked after each announce attempt.
// onTooManyFails is called when a torrent exceeds the configured failure limit.
func NewScheduler(
	port int,
	jitterPct int,
	client *ClientConfig,
	cfg *config.Config,
	proxyURL string,
	onSuccess func(infoHashHex string, resp *AnnounceResponse),
	onFailure func(infoHashHex string, err error),
	onTooManyFails func(infoHashHex string),
	getUploaded func(infoHashHex string) int64,
) *Scheduler {
	transport := NewUTLSTransport(ClientHelloForEmulatedClient(strings.ToLower(client.UserAgent)))
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}

	return &Scheduler{
		announcers:     make(map[string]*scheduledTorrent),
		jitterPct:      jitterPct,
		port:           port,
		client:         client,
		config:         cfg,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
		onSuccess:      onSuccess,
		onFailure:      onFailure,
		onTooManyFails: onTooManyFails,
		getUploaded:    getUploaded,
		stop:           make(chan struct{}),
	}
}

// AddTorrent registers a torrent for periodic announcing.
// The first announce (event=started) is scheduled immediately.
func (s *Scheduler) AddTorrent(t *torrent.Torrent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.announcers[t.InfoHashHex]; exists {
		return
	}

	a := newAnnouncer(t, s.client, s.httpClient)
	s.announcers[t.InfoHashHex] = &scheduledTorrent{
		announcer: a,
		nextAt:    time.Now().Add(time.Duration(rand.Intn(15)) * time.Second), // stagger 0-15s
		started:   false,
	}
}

// RemoveTorrent sends a stopped announce and removes the torrent from the
// scheduler. It is a best-effort operation — errors are delivered via onFailure.
func (s *Scheduler) RemoveTorrent(infoHashHex string) {
	s.mu.Lock()
	entry, exists := s.announcers[infoHashHex]
	if exists {
		delete(s.announcers, infoHashHex)
	}
	s.mu.Unlock()

	if !exists {
		return
	}

	t := entry.announcer.torrent
	// Include final uploaded bytes in stopped announce
	uploaded := int64(0)
	if s.getUploaded != nil {
		uploaded = s.getUploaded(infoHashHex)
	}
	params := AnnounceParams{
		InfoHash: t.InfoHash,
		Port:     s.GetPort(),
		Uploaded: uploaded,
		Left:     0,
		Event:    "stopped",
	}

	resp, err := entry.announcer.Announce(params)
	if err != nil {
		if s.onFailure != nil {
			s.onFailure(infoHashHex, err)
		}
		return
	}
	if s.onSuccess != nil {
		s.onSuccess(infoHashHex, resp)
	}
}

// Run starts the scheduling loop and blocks until ctx is cancelled or Stop is
// called. It should be run in its own goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// Stop signals the scheduler to stop its Run loop. Safe to call multiple times.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// SetPort updates the port used in future announces (for port rotation).
func (s *Scheduler) SetPort(port int) {
	s.portMu.Lock()
	s.port = port
	s.portMu.Unlock()
}

// GetPort returns the current announce port.
func (s *Scheduler) GetPort() int {
	s.portMu.RLock()
	defer s.portMu.RUnlock()
	return s.port
}

// tick iterates over all registered torrents and announces any that are due.
func (s *Scheduler) tick() {
	now := time.Now()

	// Collect due entries under the read lock; announce outside the lock so
	// that AddTorrent / RemoveTorrent are not blocked during network I/O.
	s.mu.RLock()
	var due []string
	for hash, entry := range s.announcers {
		entry.mu.Lock()
		paused := entry.paused
		nextAt := entry.nextAt
		entry.mu.Unlock()
		if !paused && !now.Before(nextAt) {
			due = append(due, hash)
		}
	}
	s.mu.RUnlock()

	// Announce all due torrents in parallel to avoid blocking on slow trackers.
	var wg sync.WaitGroup
	for _, hash := range due {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			s.announceOne(h)
		}(hash)
	}
	wg.Wait()
}

// announceOne performs a single announce for the torrent identified by hash.
// It uses a per-entry mutex to guard field access, holding it only while
// reading/writing fields — never during network I/O.
func (s *Scheduler) announceOne(infoHashHex string) {
	s.mu.RLock()
	entry, exists := s.announcers[infoHashHex]
	s.mu.RUnlock()

	if !exists {
		return
	}

	// Guard against double-announce when a slow tracker causes the next tick
	// to fire before the previous announce goroutine has finished.
	entry.mu.Lock()
	if entry.announcing {
		entry.mu.Unlock()
		return
	}
	entry.announcing = true
	// Snapshot fields needed to build the announce params.
	started := entry.started
	downloadedSoFar := entry.downloadedSoFar
	completedSent := entry.completedSent
	entry.mu.Unlock()

	t := entry.announcer.torrent
	event := ""
	if !started {
		event = "started"
	}

	left := int64(0)
	downloaded := int64(0)
	if s.config != nil && s.config.SimulateDownload {
		left = t.Size - downloadedSoFar
		if left < 0 {
			left = 0
		}
		downloaded = downloadedSoFar
		// Send completed event when download finishes for the first time.
		if left == 0 && !completedSent && started {
			event = "completed"
		}
	}

	announceIP := ""
	if s.config != nil {
		announceIP = s.config.AnnounceIP
	}

	// Get the accumulated uploaded bytes from the dispatcher
	uploaded := int64(0)
	if s.getUploaded != nil {
		uploaded = s.getUploaded(infoHashHex)
	}

	params := AnnounceParams{
		InfoHash:   t.InfoHash,
		Port:       s.GetPort(),
		Uploaded:   uploaded,
		Downloaded: downloaded,
		Left:       left,
		Event:      event,
		IP:         announceIP,
	}

	// Network I/O — no locks held.
	resp, err := entry.announcer.Announce(params)
	if err != nil {
		if s.onFailure != nil {
			s.onFailure(infoHashHex, err)
		}
		// Back off on consecutive failures using the default interval.
		s.mu.Lock()
		_, stillTracked := s.announcers[infoHashHex]
		s.mu.Unlock()

		entry.mu.Lock()
		entry.announcing = false
		if stillTracked {
			entry.consecutiveFails++
			backoff := applyJitter(entry.announcer.interval, s.jitterPct)
			entry.nextAt = time.Now().Add(backoff)
		}
		maxFails := 0
		if s.config != nil {
			maxFails = s.config.MaxAnnounceFailures
		}
		tooMany := stillTracked && maxFails > 0 && entry.consecutiveFails >= maxFails
		entry.mu.Unlock()

		if tooMany {
			s.mu.Lock()
			delete(s.announcers, infoHashHex)
			s.mu.Unlock()
			if s.onTooManyFails != nil {
				s.onTooManyFails(infoHashHex)
			}
		}
		return
	}

	if s.onSuccess != nil {
		s.onSuccess(infoHashHex, resp)
	}

	entry.mu.Lock()
	entry.announcing = false
	entry.started = true
	entry.consecutiveFails = 0
	if left == 0 && !entry.completedSent {
		entry.completedSent = true
	}
	interval := resp.Interval
	if interval <= 0 {
		interval = entry.announcer.interval
	}
	entry.nextAt = time.Now().Add(applyJitter(interval, s.jitterPct))
	// Advance simulated download progress: download speed ~= upload speed * 3.
	// Use the interval as the elapsed time window for this announce cycle.
	if s.config != nil && s.config.SimulateDownload && t.Size > entry.downloadedSoFar {
		downloadSpeed := (s.config.MaxUploadRate * 1000) * 3
		elapsed := int64(interval)
		gained := downloadSpeed * elapsed
		entry.downloadedSoFar += gained
		if entry.downloadedSoFar > t.Size {
			entry.downloadedSoFar = t.Size
		}
	}
	entry.mu.Unlock()
}

// PauseTorrent marks a torrent as paused, preventing future announces until resumed.
// No "stopped" event is sent to the tracker.
func (s *Scheduler) PauseTorrent(infoHashHex string) {
	s.mu.RLock()
	entry, ok := s.announcers[infoHashHex]
	s.mu.RUnlock()
	if ok {
		entry.mu.Lock()
		entry.paused = true
		entry.mu.Unlock()
	}
}

// ResumeTorrent unpauses a torrent and schedules the next announce immediately.
func (s *Scheduler) ResumeTorrent(infoHashHex string) {
	s.mu.RLock()
	entry, ok := s.announcers[infoHashHex]
	s.mu.RUnlock()
	if ok {
		entry.mu.Lock()
		entry.paused = false
		entry.nextAt = time.Now()
		entry.mu.Unlock()
	}
}

// HasTorrent reports whether the given torrent is registered with the scheduler.
func (s *Scheduler) HasTorrent(infoHashHex string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.announcers[infoHashHex]
	return ok
}

// IsPaused reports whether the given torrent is currently paused.
func (s *Scheduler) IsPaused(infoHashHex string) bool {
	s.mu.RLock()
	entry, ok := s.announcers[infoHashHex]
	s.mu.RUnlock()
	if ok {
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return entry.paused
	}
	return false
}

// applyJitter applies a random ±jitterPct% variance to the given interval
// in seconds, returning a time.Duration.
func applyJitter(intervalSecs int, jitterPct int) time.Duration {
	if jitterPct <= 0 || intervalSecs <= 0 {
		return time.Duration(intervalSecs) * time.Second
	}

	// Random factor in [-jitterPct, +jitterPct].
	jitter := rand.Intn(2*jitterPct+1) - jitterPct
	adjusted := float64(intervalSecs) * (1.0 + float64(jitter)/100.0)
	if adjusted < 1 {
		adjusted = 1
	}
	return time.Duration(adjusted * float64(time.Second))
}
