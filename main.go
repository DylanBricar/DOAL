package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"doal/announce"
	"doal/bandwidth"
	"doal/config"
	"doal/dht"
	"doal/peerwire"
	"doal/persistence"
	"doal/torrent"
	"doal/web"
)

// fetchPublicIP tries multiple providers to detect the public IP address.
func fetchPublicIP() string {
	providers := []string{
		"https://api.ipify.org",
		"https://checkip.amazonaws.com",
		"https://icanhazip.com",
		"https://ifconfig.me/ip",
		"http://ident.me/",
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, url := range providers {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if ip != "" && len(ip) < 46 { // valid IPv4 or IPv6
			return ip
		}
	}
	return ""
}

// Engine holds all running subsystems and coordinates start/stop.
type Engine struct {
	cfg          *config.Config
	confDir      string
	watcher      *torrent.Watcher
	handlers     *web.Handlers
	clientsDir   string
	torrentsDir  string

	seeding        bool
	mu             sync.RWMutex
	dispatcher     *bandwidth.Dispatcher
	scheduler      *announce.Scheduler
	peerWire       *peerwire.Server
	dhtNode        *dht.Node
	clientConfig   *announce.ClientConfig
	cancelSeed     context.CancelFunc
	cancelWatch    context.CancelFunc

	announceStatesMu sync.Mutex                  // protects announceStates only
	announceStates   map[string]*web.AnnounceState // infoHashHex -> state
}

var _ web.EngineController = (*Engine)(nil)

func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.seeding {
		return nil
	}

	cfg := e.cfg

	// Rotate client on restart if configured.
	if cfg.RotateClientOnRestart {
		clients := e.GetClientFiles()
		if len(clients) > 1 {
			current := cfg.Client
			for i := 0; i < 100; i++ {
				pick := clients[rand.Intn(len(clients))]
				if pick != current {
					cfg.Client = pick
					e.cfg = cfg
					confPath := filepath.Join(e.confDir, "config.json")
					cfg.SaveTo(confPath)
					break
				}
			}
		}
	}

	// Load client emulation config
	clientPath := filepath.Join(e.clientsDir, cfg.Client)
	cc, err := announce.LoadClientConfig(clientPath)
	if err != nil {
		return fmt.Errorf("engine: loading client %q: %w", cfg.Client, err)
	}
	e.clientConfig = cc

	// Load persisted upload stats
	statsPath := filepath.Join(e.confDir, "upload-stats.txt")
	prevUploaded := persistence.LoadUploadStats(statsPath)

	// Create bandwidth dispatcher
	var sp bandwidth.SpeedProvider
	if cfg.SpeedModel == config.SpeedModelOrganic {
		sp = bandwidth.NewOrganicSpeedProvider(cfg.MinUploadRate*1000, cfg.MaxUploadRate*1000)
	} else {
		sp = bandwidth.NewRandomSpeedProvider(cfg.MinUploadRate*1000, cfg.MaxUploadRate*1000)
	}

	var disp *bandwidth.Dispatcher
	disp = bandwidth.NewDispatcher(cfg, sp, func(speeds map[string]int64, totalUploaded int64) {
		if e.handlers != nil && disp != nil {
			uploaded := disp.UploadedPerTorrent()
			e.handlers.BroadcastSeedingSpeed(speeds, totalUploaded, uploaded)
			// Also send tracker stats every tick
			trackerStats := e.GetTrackerStats()
			if len(trackerStats) > 0 {
				e.handlers.BroadcastTrackerStats(trackerStats)
			}
		}
		persistence.SaveUploadStats(statsPath, totalUploaded)
	})
	disp.SetTotalUploaded(prevUploaded)
	e.dispatcher = disp

	// Register existing torrents
	for _, t := range e.watcher.GetTorrents() {
		e.dispatcher.RegisterTorrent(t.InfoHashHex, t.Size)
	}

	// Start dispatcher
	seedCtx, cancelSeed := context.WithCancel(context.Background())
	e.cancelSeed = cancelSeed
	go e.dispatcher.Run()

	// Auto-detect public IP if not configured
	if cfg.AnnounceIP == "" {
		if ip := fetchPublicIP(); ip != "" {
			fmt.Printf("engine: public IP detected: %s\n", ip)
		}
	} else {
		fmt.Printf("engine: using configured IP: %s\n", cfg.AnnounceIP)
	}

	// Pick a random listen port in the range typical for qBittorrent / uTorrent.
	listenPort := 10000 + rand.Intn(55000)

	// Determine proxy URL from config.
	proxyURL := ""
	if cfg.ProxyEnabled && cfg.ProxyURL != "" {
		proxyURL = cfg.ProxyURL
	}

	// Start announce scheduler
	e.scheduler = announce.NewScheduler(listenPort, cfg.AnnounceJitterPercent, cc, cfg, proxyURL,
		func(infoHashHex string, resp *announce.AnnounceResponse) {
			fmt.Printf("announce: %s OK - S:%d L:%d interval:%ds\n", infoHashHex[:12], resp.Seeders, resp.Leechers, resp.Interval)
			e.dispatcher.UpdatePeers(infoHashHex, resp.Seeders, resp.Leechers)
			now := time.Now().Format(time.RFC3339)
			// Find the torrent, store state, broadcast to UI
			for _, t := range e.watcher.GetTorrents() {
				if t.InfoHashHex == infoHashHex {
					e.announceStatesMu.Lock()
					if e.announceStates != nil {
						e.announceStates[infoHashHex] = &web.AnnounceState{
							InfoHashHex: infoHashHex,
							Name:        t.Name,
							Size:        t.Size,
							Seeders:     resp.Seeders,
							Leechers:    resp.Leechers,
							Interval:    resp.Interval,
							AnnouncedAt: now,
						}
					}
					e.announceStatesMu.Unlock()
					e.handlers.BroadcastAnnounceSuccess(infoHashHex, t, resp.Seeders, resp.Leechers, resp.Interval)
					break
				}
			}
		},
		func(infoHashHex string, err error) {
			fmt.Printf("announce: %s FAIL - %v\n", infoHashHex[:12], err)
			e.handlers.BroadcastAnnounceFailed(infoHashHex, err.Error())
		},
		func(infoHashHex string) {
			fmt.Printf("announce: %s removed — too many consecutive failures\n", infoHashHex[:12])
			e.handlers.BroadcastTooManyFails(infoHashHex)
			e.mu.RLock()
			disp := e.dispatcher
			pw := e.peerWire
			e.mu.RUnlock()
			if disp != nil {
				disp.UnregisterTorrent(infoHashHex)
			}
			if pw != nil {
				pw.UnregisterTorrent(infoHashHex)
			}
		},
		// getUploaded: fetch per-torrent uploaded bytes from the dispatcher
		func(infoHashHex string) int64 {
			if disp != nil {
				uploaded := disp.UploadedPerTorrent()
				return uploaded[infoHashHex]
			}
			return 0
		},
	)

	torrents := e.watcher.GetTorrents()
	for i, t := range torrents {
		if i < cfg.SimultaneousSeed {
			e.scheduler.AddTorrent(t)
		}
	}
	go e.scheduler.Run(seedCtx)

	// Torrent rotation: when more torrents exist than simultaneous slots,
	// periodically swap one active torrent for an inactive one.
	if len(torrents) > cfg.SimultaneousSeed {
		go func() {
			ticker := time.NewTicker(30 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-seedCtx.Done():
					return
				case <-ticker.C:
					e.rotateTorrents()
				}
			}
		}()
	}

	// Port rotation: change port every 30-60 min if enabled
	if cfg.EnablePortRotation {
		go func() {
			for {
				delay := time.Duration(30+rand.Intn(31)) * time.Minute
				select {
				case <-seedCtx.Done():
					return
				case <-time.After(delay):
					newPort := 10000 + rand.Intn(50000)
					e.scheduler.SetPort(newPort)
					fmt.Printf("engine: port rotated to %d\n", newPort)
				}
			}
		}()
	}

	// Start PeerWire server on the same port advertised to trackers.
	e.peerWire = peerwire.NewServer(listenPort, cfg.PeerResponseMode, cc.UserAgent)
	for _, t := range torrents {
		e.peerWire.RegisterTorrent(peerwire.TorrentInfo{
			InfoHash:   t.InfoHash,
			PieceCount: t.PieceCount,
			PeerID:     []byte(cc.PeerID),
		})
	}

	// Auto-detect real data files in torrents/ directory for SHA-1 verified piece serving.
	// Convention: place the real file next to its .torrent with matching base name.
	// Example: torrents/MyMovie.1080p.torrent + torrents/MyMovie.1080p.mkv
	for _, t := range torrents {
		// Strip .torrent extension to get the base name
		torrentBase := strings.TrimSuffix(filepath.Base(t.FilePath), ".torrent")
		// Look for any non-.torrent file with the same base name
		entries, _ := os.ReadDir(e.torrentsDir)
		for _, entry := range entries {
			if entry.IsDir() || strings.HasSuffix(entry.Name(), ".torrent") {
				continue
			}
			entryBase := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			if entryBase == torrentBase {
				dataPath := filepath.Join(e.torrentsDir, entry.Name())
				info, _ := entry.Info()
				if info != nil && info.Size() == t.Size {
					e.peerWire.RegisterDataFile(t.InfoHashHex, dataPath, t.PieceLength)
					fmt.Printf("engine: SHA-1 data registered: %s -> %s (%d pieces)\n",
						t.Name, entry.Name(), t.PieceCount)
				}
				break
			}
		}
	}
	go func() {
		if err := e.peerWire.Start(); err != nil {
			fmt.Printf("engine: peerwire start: %v\n", err)
		}
	}()

	// Start minimal DHT node on listenPort+1 when peer connections are active.
	if cfg.PeerResponseMode != config.PeerResponseModeNone {
		e.dhtNode = dht.NewNode(listenPort + 1)
		for _, t := range torrents {
			e.dhtNode.AddTorrent(t.InfoHashHex)
		}
		if err := e.dhtNode.Start(); err != nil {
			fmt.Printf("engine: DHT start failed (non-fatal): %v\n", err)
			e.dhtNode = nil
		} else {
			fmt.Printf("engine: DHT node started on port %d\n", listenPort+1)
		}
	}

	e.announceStates = make(map[string]*web.AnnounceState)
	e.seeding = true
	fmt.Printf("engine: seeding started (%d torrents, client=%s)\n", len(torrents), cfg.Client)
	return nil
}

func (e *Engine) GetActiveClient() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.cfg != nil {
		return e.cfg.Client
	}
	return ""
}

func (e *Engine) GetSpeeds() (map[string]int64, int64, map[string]int64) {
	e.mu.RLock()
	d := e.dispatcher
	e.mu.RUnlock()
	if d == nil {
		return nil, 0, nil
	}
	speeds := d.GetSpeedSnapshot()
	total := d.TotalUploaded()
	uploaded := d.UploadedPerTorrent()
	return speeds, total, uploaded
}

func (e *Engine) GetAnnounceStates() []web.AnnounceState {
	e.announceStatesMu.Lock()
	defer e.announceStatesMu.Unlock()
	result := make([]web.AnnounceState, 0, len(e.announceStates))
	for _, a := range e.announceStates {
		result = append(result, *a)
	}
	return result
}

func (e *Engine) PauseTorrent(infoHashHex string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.dispatcher != nil { e.dispatcher.PauseTorrent(infoHashHex) }
	if e.scheduler != nil { e.scheduler.PauseTorrent(infoHashHex) }
}

func (e *Engine) ResumeTorrent(infoHashHex string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.dispatcher != nil { e.dispatcher.ResumeTorrent(infoHashHex) }
	if e.scheduler != nil { e.scheduler.ResumeTorrent(infoHashHex) }
}

func (e *Engine) PauseTracker(domain string) {
	for _, t := range e.watcher.GetTorrents() {
		for _, u := range t.AnnounceURLs {
			if strings.Contains(u, domain) {
				e.PauseTorrent(t.InfoHashHex)
				break
			}
		}
	}
}

func (e *Engine) ResumeTracker(domain string) {
	for _, t := range e.watcher.GetTorrents() {
		for _, u := range t.AnnounceURLs {
			if strings.Contains(u, domain) {
				e.ResumeTorrent(t.InfoHashHex)
				break
			}
		}
	}
}

func (e *Engine) GetPausedTorrents() map[string]bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]bool)
	if e.scheduler == nil { return result }
	for _, t := range e.watcher.GetTorrents() {
		if e.scheduler.IsPaused(t.InfoHashHex) {
			result[t.InfoHashHex] = true
		}
	}
	return result
}

func (e *Engine) GetTrackerStats() map[string]int64 {
	e.mu.RLock()
	d := e.dispatcher
	e.mu.RUnlock()
	if d == nil { return nil }
	uploaded := d.UploadedPerTorrent()
	stats := make(map[string]int64)
	for _, t := range e.watcher.GetTorrents() {
		if len(t.AnnounceURLs) > 0 {
			// Extract domain from first announce URL
			u := t.AnnounceURLs[0]
			domain := u
			if idx := strings.Index(u, "://"); idx >= 0 {
				domain = u[idx+3:]
			}
			if idx := strings.Index(domain, "/"); idx >= 0 {
				domain = domain[:idx]
			}
			stats[domain] += uploaded[t.InfoHashHex]
		}
	}
	return stats
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.seeding {
		e.mu.Unlock()
		return
	}
	e.seeding = false
	sched := e.scheduler
	torrents := e.watcher.GetTorrents()
	e.mu.Unlock()

	// Send stopped announces concurrently outside the lock to avoid deadlock
	// (announce callbacks also acquire e.mu).
	if sched != nil {
		var wg sync.WaitGroup
		for _, t := range torrents {
			wg.Add(1)
			go func(hash string) {
				defer wg.Done()
				sched.RemoveTorrent(hash)
			}(t.InfoHashHex)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			fmt.Println("engine: stopped announce timeout, continuing shutdown")
		}
	}

	e.mu.Lock()

	if e.cancelSeed != nil {
		e.cancelSeed()
	}
	if e.peerWire != nil {
		e.peerWire.Stop()
		e.peerWire = nil
	}
	if e.dhtNode != nil {
		e.dhtNode.Stop()
		e.dhtNode = nil
	}
	if e.dispatcher != nil {
		e.dispatcher.Stop()
		e.dispatcher = nil
	}
	// Reset upload stats to 0 for the next session
	statsPath := filepath.Join(e.confDir, "upload-stats.txt")
	persistence.SaveUploadStats(statsPath, 0)
	e.scheduler = nil
	e.clientConfig = nil
	e.mu.Unlock()

	e.announceStatesMu.Lock()
	e.announceStates = nil
	e.announceStatesMu.Unlock()

	fmt.Println("engine: seeding stopped")
}

// rotateTorrents swaps one active torrent for one inactive torrent to ensure
// all torrents get seeding time when simultaneousSeed < total torrents.
func (e *Engine) rotateTorrents() {
	allTorrents := e.watcher.GetTorrents()

	e.mu.RLock()
	sched := e.scheduler
	disp := e.dispatcher
	simultaneousSeed := e.cfg.SimultaneousSeed
	e.mu.RUnlock()

	if sched == nil || len(allTorrents) <= simultaneousSeed {
		return
	}

	var active []string
	var inactive []*torrent.Torrent
	for _, t := range allTorrents {
		if sched.HasTorrent(t.InfoHashHex) && !sched.IsPaused(t.InfoHashHex) {
			active = append(active, t.InfoHashHex)
		} else {
			inactive = append(inactive, t)
		}
	}

	if len(active) == 0 || len(inactive) == 0 {
		return
	}

	removeHash := active[rand.Intn(len(active))]
	addTorrent := inactive[rand.Intn(len(inactive))]

	sched.RemoveTorrent(removeHash)
	if disp != nil {
		disp.UnregisterTorrent(removeHash)
	}

	sched.AddTorrent(addTorrent)
	if disp != nil {
		disp.RegisterTorrent(addTorrent.InfoHashHex, addTorrent.Size)
	}

	fmt.Printf("engine: rotated torrents — removed %s, added %s\n", removeHash[:12], addTorrent.InfoHashHex[:12])
}

// SaveConfig validates and persists cfg, then updates the engine's active config.
func (e *Engine) SaveConfig(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("engine: invalid config: %w", err)
	}

	confPath := filepath.Join(e.confDir, "config.json")
	if err := cfg.SaveTo(confPath); err != nil {
		return fmt.Errorf("engine: saving config: %w", err)
	}

	e.mu.Lock()
	e.cfg = cfg
	// If seeding, update the dispatcher's config live
	if e.seeding && e.dispatcher != nil {
		e.dispatcher.UpdateConfig(cfg)
	}
	e.mu.Unlock()

	return nil
}

// GetConfig returns a snapshot of the current configuration.
func (e *Engine) GetConfig() *config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// GetClientFiles returns the list of .client filenames available in the clients directory.
func (e *Engine) GetClientFiles() []string {
	entries, err := os.ReadDir(e.clientsDir)
	if err != nil {
		fmt.Printf("engine: reading clients dir %q: %v\n", e.clientsDir, err)
		return nil
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".client" {
			files = append(files, entry.Name())
		}
	}
	return files
}

// GetTorrents returns a snapshot of all currently tracked torrents.
func (e *Engine) GetTorrents() []*torrent.Torrent {
	if e.watcher == nil {
		return nil
	}
	return e.watcher.GetTorrents()
}

// IsSeeding reports whether the engine is currently seeding.
func (e *Engine) IsSeeding() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.seeding
}

// TorrentsDir returns the directory where .torrent files are stored.
func (e *Engine) TorrentsDir() string {
	return e.torrentsDir
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var (
		confDir     = flag.String("conf", "", "path to config directory (required)")
		port        = flag.Int("port", 5081, "web server port")
		pathPrefix  = flag.String("path-prefix", "doal", "URL path prefix")
		secretToken = flag.String("secret-token", "", "auth token for WebSocket (required)")
	)
	flag.Parse()

	if *confDir == "" {
		slog.Error("--conf is required")
		flag.Usage()
		os.Exit(1)
	}
	if *secretToken == "" {
		slog.Error("--secret-token is required")
		flag.Usage()
		os.Exit(1)
	}

	absConf, err := filepath.Abs(*confDir)
	if err != nil {
		slog.Error("resolving conf path", "err", err)
		os.Exit(1)
	}

	confPath := filepath.Join(absConf, "config.json")
	cfg, err := config.Load(confPath)
	if err != nil {
		slog.Error("loading config", "err", err)
		os.Exit(1)
	}

	torrentsDir := filepath.Join(absConf, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		slog.Error("creating torrents dir", "err", err)
		os.Exit(1)
	}

	clientsDir := filepath.Join(absConf, "clients")

	engine := &Engine{
		cfg:         cfg,
		confDir:     absConf,
		clientsDir:  clientsDir,
		torrentsDir: torrentsDir,
	}

	// Create WebSocket server (onMessage wired by NewHandlers below).
	srv := web.NewServer(*port, *pathPrefix, *secretToken, nil)

	// Wire handlers: sets srv.onMessage internally.
	handlers := web.NewHandlers(srv, engine)
	engine.handlers = handlers

	// Create torrent watcher.
	watcher, err := torrent.NewWatcher(torrentsDir)
	if err != nil {
		slog.Error("creating torrent watcher", "err", err)
		os.Exit(1)
	}
	engine.watcher = watcher

	// Set watcher callbacks to broadcast events.
	watcher.OnAdd = func(t *torrent.Torrent) {
		slog.Info("torrent added", "name", t.Name, "hash", t.InfoHashHex)
		handlers.BroadcastTorrentAdded(t)
	}
	watcher.OnRemove = func(t *torrent.Torrent) {
		slog.Info("torrent removed", "name", t.Name, "hash", t.InfoHashHex)
		handlers.BroadcastTorrentDeleted(t)
		engine.mu.RLock()
		seeding := engine.seeding
		disp := engine.dispatcher
		sched := engine.scheduler
		pw := engine.peerWire
		engine.mu.RUnlock()
		if seeding {
			if disp != nil { disp.UnregisterTorrent(t.InfoHashHex) }
			if sched != nil { go sched.RemoveTorrent(t.InfoHashHex) } // async to avoid blocking
			if pw != nil { pw.UnregisterTorrent(t.InfoHashHex) }
		}
	}

	// Scan existing .torrent files before starting the watch loop.
	if err := watcher.ScanExisting(); err != nil {
		slog.Warn("scanning existing torrents", "err", err)
	}

	// Start watcher in background.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	engine.cancelWatch = cancelWatch
	go watcher.Start(watchCtx)

	// Handle OS signals for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		engine.Stop()
		cancelWatch()
		os.Exit(0)
	}()

	// Start HTTP server (blocks).
	if err := srv.Start(); err != nil {
		slog.Error("web server", "err", err)
		os.Exit(1)
	}
}

