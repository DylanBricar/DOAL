package announce

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"hash/fnv"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"doal/config"
	"doal/torrent"
)

const maxConcurrentAnnounces = 32

// scheduledTorrent tracks an Announcer alongside its next scheduled announce time.
type scheduledTorrent struct {
	mu               sync.Mutex
	announcer        *Announcer
	nextAt           time.Time
	started          bool
	paused           bool
	download         *downloadLifecycle
	ring             *matchedRing
	rng              *rand.Rand
	consecutiveFails int
	announcing       bool // prevents double-announce when tick fires before previous finishes
	announceDone     chan struct{}
	removed          bool
}

func (e *scheduledTorrent) finishAnnounceLocked() {
	e.announcing = false
	if e.announceDone != nil {
		close(e.announceDone)
		e.announceDone = nil
	}
}

// Scheduler manages periodic announces for a set of torrents.
// It honours per-torrent intervals with configurable jitter.
type Scheduler struct {
	announcers     map[string]*scheduledTorrent // infoHashHex -> entry
	mu             sync.RWMutex
	jitterPct      int
	port           int
	portMu         sync.RWMutex
	client         *ClientConfig
	config         *config.Config
	httpClient     *http.Client
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
		announcers: make(map[string]*scheduledTorrent),
		jitterPct:  jitterPct,
		port:       port,
		client:     client,
		config:     cfg,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				if !IsSupportedTrackerURL(req.URL.String()) {
					return http.ErrUseLastResponse
				}
				return nil
			},
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

	torrentClient := s.client.clone()
	a := newAnnouncer(t, torrentClient, s.httpClient)
	now := time.Now()
	seed := schedulerSeed(t.InfoHashHex)
	rng := rand.New(rand.NewSource(seed))
	minDownloadRate := int64(1)
	maxDownloadRate := int64(1)
	if s.config != nil {
		minDownloadRate = s.config.MinUploadRate * 1000
		maxDownloadRate = s.config.MaxUploadRate * 3000
	}
	entry := &scheduledTorrent{
		announcer: a,
		nextAt:    now.Add(time.Duration(rng.Intn(16)) * time.Second),
		started:   false,
		download:  newDownloadLifecycle(t.Size, minDownloadRate, maxDownloadRate, now, seed^0x5deece66d),
		rng:       rng,
	}
	if s.config != nil && s.config.EnableLabSybilRing {
		baselineUploaded := int64(0)
		if s.getUploaded != nil {
			baselineUploaded = s.getUploaded(t.InfoHashHex)
		}
		ring, err := newMatchedRing(
			t,
			torrentClient,
			s.httpClient,
			s.config.LabSybilPeers,
			s.GetPort(),
			s.config.AnnounceIP,
			baselineUploaded,
		)
		if err != nil {
			if s.onFailure != nil {
				s.onFailure(t.InfoHashHex, err)
			}
		} else {
			entry.ring = ring
		}
	}
	s.announcers[t.InfoHashHex] = entry
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
	entry.mu.Lock()
	entry.removed = true
	announceDone := entry.announceDone
	entry.mu.Unlock()
	if announceDone != nil {
		<-announceDone
	}
	if entry.ring != nil {
		defer func() {
			if err := entry.ring.stop(); err != nil && s.onFailure != nil {
				s.onFailure(infoHashHex, err)
			}
		}()
	}

	t := entry.announcer.torrent
	// Include final transfer counters in the stopped announce.
	uploaded := int64(0)
	if s.getUploaded != nil {
		uploaded = s.getUploaded(infoHashHex)
	}
	downloaded := int64(0)
	left := int64(0)
	if s.config != nil && s.config.SimulateDownload && entry.download != nil {
		entry.mu.Lock()
		downloaded, left, _ = entry.download.peek(time.Now())
		entry.mu.Unlock()
	}
	params := AnnounceParams{
		InfoHash:   t.InfoHash,
		Port:       s.GetPort(),
		Uploaded:   uploaded,
		Downloaded: downloaded,
		Left:       left,
		Event:      "stopped",
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
	if entry.ring != nil {
		if err := entry.ring.matchUploaded(uploaded); err != nil && s.onFailure != nil {
			s.onFailure(infoHashHex, err)
		}
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

	// Bound both goroutine count and network concurrency when many torrents are
	// due at once.
	if len(due) == 0 {
		return
	}
	workerCount := min(len(due), maxConcurrentAnnounces)
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hash := range jobs {
				s.announceOne(hash)
			}
		}()
	}
	for _, hash := range due {
		jobs <- hash
	}
	close(jobs)
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
	if entry.announcing || entry.removed {
		entry.mu.Unlock()
		return
	}
	entry.announcing = true
	entry.announceDone = make(chan struct{})
	// Snapshot fields needed to build the announce params.
	started := entry.started
	download := entry.download
	entry.mu.Unlock()

	t := entry.announcer.torrent
	event := ""
	if !started {
		event = "started"
	}

	left := int64(0)
	downloaded := int64(0)
	completedTransition := false
	if s.config != nil && s.config.SimulateDownload && download != nil {
		entry.mu.Lock()
		downloaded, left, completedTransition = download.peek(time.Now())
		entry.mu.Unlock()
		// Send completed event when download finishes for the first time.
		if completedTransition && started {
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
		if stillTracked {
			entry.consecutiveFails++
			backoff := applyJitterWithRand(entry.announcer.interval, s.jitterPct, entry.rng)
			entry.nextAt = time.Now().Add(backoff)
		}
		maxFails := 0
		if s.config != nil {
			maxFails = s.config.MaxAnnounceFailures
		}
		tooMany := stillTracked && maxFails > 0 && entry.consecutiveFails >= maxFails
		if tooMany {
			entry.removed = true
		}
		entry.finishAnnounceLocked()
		entry.mu.Unlock()

		if tooMany {
			s.mu.Lock()
			delete(s.announcers, infoHashHex)
			s.mu.Unlock()
			if s.onTooManyFails != nil {
				s.onTooManyFails(infoHashHex)
			}
			if entry.ring != nil {
				if err := entry.ring.stop(); err != nil && s.onFailure != nil {
					s.onFailure(infoHashHex, err)
				}
			}
		}
		return
	}

	if s.onSuccess != nil {
		s.onSuccess(infoHashHex, resp)
	}
	if entry.ring != nil {
		if err := entry.ring.matchUploaded(uploaded); err != nil && s.onFailure != nil {
			s.onFailure(infoHashHex, err)
		}
	}

	entry.mu.Lock()
	entry.started = true
	entry.consecutiveFails = 0
	if completedTransition && event == "completed" && entry.download != nil {
		entry.download.markCompletedEmitted()
	}
	interval := resp.Interval
	if interval <= 0 {
		interval = entry.announcer.interval
	}
	entry.nextAt = time.Now().Add(applyJitterWithRand(interval, s.jitterPct, entry.rng))
	entry.finishAnnounceLocked()
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
	return applyJitterWithRand(intervalSecs, jitterPct, nil)
}

func applyJitterWithRand(intervalSecs int, jitterPct int, rng *rand.Rand) time.Duration {
	if jitterPct <= 0 || intervalSecs <= 0 {
		return time.Duration(intervalSecs) * time.Second
	}

	// Random factor in [-jitterPct, +jitterPct].
	jitter := 0
	if rng == nil {
		jitter = rand.Intn(2*jitterPct+1) - jitterPct
	} else {
		jitter = rng.Intn(2*jitterPct+1) - jitterPct
	}
	adjusted := float64(intervalSecs) * (1.0 + float64(jitter)/100.0)
	if adjusted < 1 {
		adjusted = 1
	}
	return time.Duration(adjusted * float64(time.Second))
}

func schedulerSeed(infoHashHex string) int64 {
	var raw [8]byte
	if _, err := cryptorand.Read(raw[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(raw[:]))
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(infoHashHex))
	return int64(h.Sum64() ^ uint64(time.Now().UnixNano()))
}
