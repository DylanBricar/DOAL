package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
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
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return fetchPublicIPFrom(ctx, &http.Client{}, providers)
}

func fetchPublicIPFrom(ctx context.Context, client *http.Client, providers []string) string {
	results := make(chan string, len(providers))
	var wg sync.WaitGroup
	for _, provider := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				return
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 129))
			if err != nil || len(body) > 128 {
				return
			}
			ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
			if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
				return
			}
			select {
			case results <- ip.String():
			case <-ctx.Done():
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	for {
		select {
		case ip, ok := <-results:
			if !ok {
				return ""
			}
			return ip
		case <-ctx.Done():
			return ""
		}
	}
}

// Engine holds all running subsystems and coordinates start/stop.
type Engine struct {
	cfg         *config.Config
	confDir     string
	watcher     *torrent.Watcher
	handlers    *web.Handlers
	clientsDir  string
	torrentsDir string

	seeding      bool
	mu           sync.RWMutex
	lifecycleMu  sync.Mutex
	slotsMu      sync.Mutex
	configSaveMu sync.Mutex
	failedSlots  map[string]bool // protected by slotsMu; retried on explicit resume or restart
	pausedSlots  map[string]bool // protected by slotsMu; resumed only by explicit user action or restart
	dispatcher   *bandwidth.Dispatcher
	scheduler    *announce.Scheduler
	peerWire     *peerwire.Server
	dhtNode      *dht.Node
	clientConfig *announce.ClientConfig
	cancelSeed   context.CancelFunc
	cancelWatch  context.CancelFunc

	announceStatesMu sync.Mutex                    // protects announceStates only
	announceStates   map[string]*web.AnnounceState // infoHashHex -> state
}

var _ web.EngineController = (*Engine)(nil)

func (e *Engine) Start() error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.configSaveMu.Lock()
	defer e.configSaveMu.Unlock()
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
					rotated := cloneConfig(cfg)
					rotated.Client = pick
					confPath := filepath.Join(e.confDir, "config.json")
					if err := rotated.SaveTo(confPath); err != nil {
						return fmt.Errorf("engine: saving rotated client config: %w", err)
					}
					cfg = rotated
					e.cfg = rotated
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
	disp.SetAutoPauseCallback(func(infoHashHex string) {
		e.pauseTorrentRuntime(infoHashHex)
		if e.handlers != nil {
			e.handlers.BroadcastTorrentPaused(infoHashHex)
		}
	})
	e.dispatcher = disp

	// Create the cancellation domain now. Worker goroutines start only after the
	// advertised PeerWire port has been bound successfully.
	seedCtx, cancelSeed := context.WithCancel(context.Background())
	e.cancelSeed = cancelSeed

	// Auto-detect public IP if not configured
	if cfg.AnnounceIP == "" {
		if ip := fetchPublicIP(); ip != "" {
			ip = strings.TrimSpace(ip)
			cfg.AnnounceIP = ip
			slog.Info("public IP auto-detected", "ip", ip)
		}
	} else {
		slog.Info("using configured announce IP", "ip", cfg.AnnounceIP)
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
			slog.Info("announce ok", "hash", infoHashHex[:12], "seeders", resp.Seeders, "leechers", resp.Leechers, "interval", resp.Interval)
			disp.UpdatePeers(infoHashHex, resp.Seeders, resp.Leechers)

			// Feed real seed addresses to the piece proxy (no-op if disabled).
			e.mu.RLock()
			pw := e.peerWire
			e.mu.RUnlock()
			if pw != nil && len(resp.Peers) > 0 {
				peers := make([]peerwire.Peer, 0, len(resp.Peers))
				for _, p := range resp.Peers {
					peers = append(peers, peerwire.Peer{IP: p.IP, Port: p.Port})
				}
				pw.UpdatePeers(infoHashHex, peers)
			}

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
			slog.Warn("announce failed", "hash", infoHashHex[:12], "err", err)
			e.handlers.BroadcastAnnounceFailed(infoHashHex, err.Error())
		},
		func(infoHashHex string) {
			slog.Warn("too many announce failures, removed", "hash", infoHashHex[:12])
			e.handlers.BroadcastTooManyFails(infoHashHex)
			go e.removeFailedTorrentRuntime(infoHashHex)
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

	// Start PeerWire server on the same port advertised to trackers.
	e.peerWire = peerwire.NewServer(listenPort, cfg.PeerResponseMode, cc.UserAgent)
	if cfg.EnablePieceProxy {
		e.peerWire.EnablePieceProxyWithNetworkPolicy(cfg.AllowPrivateNetworks)
		slog.Info("piece proxy enabled (on-demand leech + SHA-1 verify)")
	}
	// Start the configured DHT first so PeerWire advertises DHT only when the UDP
	// listener is genuinely active and can publish its PORT message.
	if cfg.PeerResponseMode != config.PeerResponseModeNone {
		e.dhtNode = dht.NewNode(listenPort + 1)
		if err := e.dhtNode.ConfigureNetwork(listenPort, cfg.DHTBootstrapNodes); err != nil {
			slog.Warn("DHT configuration failed (non-fatal)", "err", err)
			e.dhtNode = nil
		}
	}

	e.failedSlots = make(map[string]bool)
	e.pausedSlots = make(map[string]bool)
	torrents := sortedTorrents(e.watcher.GetTorrents())
	for i, t := range torrents {
		if i >= cfg.SimultaneousSeed {
			break
		}
		e.activateTorrent(t, e.scheduler, disp, e.peerWire, e.dhtNode, cc)
	}
	if e.dhtNode != nil {
		if err := e.dhtNode.Start(); err != nil {
			slog.Warn("DHT start failed (non-fatal)", "err", err)
			e.dhtNode = nil
		} else {
			dhtPort := e.dhtNode.Addr().Port
			e.peerWire.EnableDHT(dhtPort)
			slog.Info("DHT node started", "port", dhtPort)
		}
	}
	if err := e.peerWire.Start(); err != nil {
		if e.dhtNode != nil {
			e.dhtNode.Stop()
		}
		e.peerWire.Stop()
		cancelSeed()
		disp.Stop()
		e.dispatcher = nil
		e.scheduler = nil
		e.peerWire = nil
		e.dhtNode = nil
		e.clientConfig = nil
		e.cancelSeed = nil
		return fmt.Errorf("engine: starting PeerWire listener: %w", err)
	}

	e.announceStates = make(map[string]*web.AnnounceState)
	e.seeding = true
	go e.dispatcher.Run()
	go e.scheduler.Run(seedCtx)
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
	slog.Info("seeding started", "torrents", len(torrents), "client", cfg.Client)
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
	e.pauseTorrentRuntime(infoHashHex)
}

func (e *Engine) ResumeTorrent(infoHashHex string) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	wasQuarantined := e.failedSlots[infoHashHex] || e.pausedSlots[infoHashHex]
	delete(e.failedSlots, infoHashHex)
	delete(e.pausedSlots, infoHashHex)
	e.mu.RLock()
	seeding := e.seeding
	sched, disp, pw, dhtNode, cc := e.scheduler, e.dispatcher, e.peerWire, e.dhtNode, e.clientConfig
	target := 0
	if e.cfg != nil {
		target = e.cfg.SimultaneousSeed
	}
	e.mu.RUnlock()
	if !seeding || sched == nil {
		return
	}
	t := e.torrentByHash(infoHashHex)
	if t == nil {
		return
	}
	if !sched.HasTorrent(infoHashHex) {
		if !wasQuarantined || cc == nil {
			return
		}
		if eviction := selectExplicitResumeEviction(sched.TorrentHashes(), target); eviction != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			e.deactivateTorrent(ctx, eviction, sched, disp, pw, dhtNode)
			cancel()
			if e.handlers != nil {
				e.handlers.BroadcastTorrentSlotDeactivated(eviction)
			}
		}
		if e.activateTorrent(t, sched, disp, pw, dhtNode, cc) {
			if e.handlers != nil {
				e.handlers.BroadcastTorrentSlotActivated(t)
			}
			return
		}
		if e.pausedSlots == nil {
			e.pausedSlots = make(map[string]bool)
		}
		e.pausedSlots[infoHashHex] = true
		e.rebalanceActiveSlotsLocked()
		return
	}
	disp.ResumeTorrent(infoHashHex)
	sched.ResumeTorrent(infoHashHex)
	if pw != nil && cc != nil {
		pw.RegisterTorrent(peerWireTorrentInfo(t, cc))
		e.registerDataFile(pw, t)
	}
	if dhtNode != nil {
		dhtNode.AddTorrent(infoHashHex)
	}
}

func (e *Engine) PauseTracker(domain string) {
	var hashes []string
	for _, t := range e.watcher.GetTorrents() {
		if torrentUsesTracker(t, domain) {
			hashes = append(hashes, t.InfoHashHex)
		}
	}
	if len(hashes) == 0 {
		return
	}
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.mu.RLock()
	seeding := e.seeding
	sched, disp, pw, dhtNode := e.scheduler, e.dispatcher, e.peerWire, e.dhtNode
	e.mu.RUnlock()
	if !seeding || sched == nil {
		return
	}
	if e.pausedSlots == nil {
		e.pausedSlots = make(map[string]bool)
	}
	for _, hash := range hashes {
		e.pausedSlots[hash] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hash := range hashes {
		if sched.HasTorrent(hash) {
			e.deactivateTorrent(ctx, hash, sched, disp, pw, dhtNode)
		}
	}
	e.rebalanceActiveSlotsLocked()
}

func (e *Engine) ResumeTracker(domain string) {
	var hashes []string
	for _, t := range e.watcher.GetTorrents() {
		if torrentUsesTracker(t, domain) {
			hashes = append(hashes, t.InfoHashHex)
		}
	}
	if len(hashes) == 0 {
		return
	}
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	for _, hash := range hashes {
		delete(e.failedSlots, hash)
		delete(e.pausedSlots, hash)
	}
	e.rebalanceActiveSlotsLocked()
}

func torrentUsesTracker(t *torrent.Torrent, authority string) bool {
	authority = strings.TrimSpace(strings.TrimSuffix(authority, "."))
	if t == nil || authority == "" {
		return false
	}
	for _, raw := range t.AnnounceURLs {
		u, err := url.Parse(raw)
		if err == nil && strings.EqualFold(strings.TrimSuffix(u.Host, "."), authority) {
			return true
		}
	}
	return false
}

func (e *Engine) GetPausedTorrents() map[string]bool {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	result := make(map[string]bool, len(e.pausedSlots))
	for hash := range e.pausedSlots {
		result[hash] = true
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.scheduler == nil {
		return result
	}
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
	if d == nil {
		return nil
	}
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
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	e.slotsMu.Lock()
	e.mu.Lock()
	if !e.seeding {
		e.mu.Unlock()
		e.slotsMu.Unlock()
		return
	}
	e.seeding = false
	e.failedSlots = nil
	e.pausedSlots = nil
	sched := e.scheduler
	cancelSeed := e.cancelSeed
	pw := e.peerWire
	dhtNode := e.dhtNode
	disp := e.dispatcher
	var activeHashes []string
	if sched != nil {
		activeHashes = sched.TorrentHashes()
	}
	e.mu.Unlock()
	e.slotsMu.Unlock()

	// Cancel and join periodic work before dismantling any dependencies. Final
	// stopped announces are bounded by a shared shutdown deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if sched != nil {
		if cancelSeed != nil {
			cancelSeed()
		}
		sched.Stop()
		sched.Wait()
		removeScheduledTorrents(ctx, sched, activeHashes)
	}

	if pw != nil {
		pw.Stop()
	}
	if dhtNode != nil {
		dhtNode.Stop()
	}
	if disp != nil {
		disp.Stop()
		disp.Wait()
	}

	e.mu.Lock()
	e.cancelSeed = nil
	if e.peerWire == pw {
		e.peerWire = nil
	}
	if e.dhtNode == dhtNode {
		e.dhtNode = nil
	}
	if e.dispatcher == disp {
		e.dispatcher = nil
	}
	if e.scheduler == sched {
		e.scheduler = nil
	}
	// Reset upload stats to 0 for the next session
	statsPath := filepath.Join(e.confDir, "upload-stats.txt")
	persistence.SaveUploadStats(statsPath, 0)
	e.clientConfig = nil
	e.mu.Unlock()

	e.announceStatesMu.Lock()
	e.announceStates = nil
	e.announceStatesMu.Unlock()

	slog.Info("seeding stopped")
}

// rotateTorrents swaps one active torrent for one inactive torrent to ensure
// all torrents get seeding time when simultaneousSeed < total torrents.
func (e *Engine) rotateTorrents() {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	allTorrents := sortedTorrents(e.watcher.GetTorrents())

	e.mu.RLock()
	sched := e.scheduler
	disp := e.dispatcher
	pw := e.peerWire
	dhtNode := e.dhtNode
	cc := e.clientConfig
	seeding := e.seeding
	simultaneousSeed := e.cfg.SimultaneousSeed
	e.mu.RUnlock()

	if !seeding || sched == nil || len(allTorrents) <= simultaneousSeed {
		return
	}

	var active []string
	var inactive []*torrent.Torrent
	for _, t := range allTorrents {
		if sched.HasTorrent(t.InfoHashHex) && !sched.IsPaused(t.InfoHashHex) {
			active = append(active, t.InfoHashHex)
		} else if !sched.HasTorrent(t.InfoHashHex) && !e.failedSlots[t.InfoHashHex] && !e.pausedSlots[t.InfoHashHex] {
			inactive = append(inactive, t)
		}
	}

	if len(active) == 0 || len(inactive) == 0 {
		return
	}

	removeHash := active[rand.Intn(len(active))]
	addTorrent := inactive[rand.Intn(len(inactive))]

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.deactivateTorrent(ctx, removeHash, sched, disp, pw, dhtNode)
	e.activateTorrent(addTorrent, sched, disp, pw, dhtNode, cc)

	slog.Info("torrent rotated", "removed", removeHash[:12], "added", addTorrent.InfoHashHex[:12])
}

// SaveConfig validates and persists cfg, then updates the engine's active config.
func (e *Engine) SaveConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("engine: config is nil")
	}
	stored := cloneConfig(cfg)
	if err := stored.Validate(); err != nil {
		return fmt.Errorf("engine: invalid config: %w", err)
	}

	e.configSaveMu.Lock()
	defer e.configSaveMu.Unlock()
	e.mu.RLock()
	if e.seeding && restartBoundConfigChanged(e.cfg, stored) {
		e.mu.RUnlock()
		return errors.New("engine: stop seeding before changing client or network settings")
	}
	e.mu.RUnlock()
	confPath := filepath.Join(e.confDir, "config.json")
	if err := stored.SaveTo(confPath); err != nil {
		return fmt.Errorf("engine: saving config: %w", err)
	}

	e.mu.Lock()
	e.cfg = stored
	// If seeding, update the dispatcher's config live
	if e.seeding && e.dispatcher != nil {
		e.dispatcher.UpdateConfig(stored)
	}
	seeding := e.seeding
	e.mu.Unlock()
	if seeding {
		go e.rebalanceActiveSlots()
	}

	return nil
}

func (e *Engine) GetActiveTorrentHashes() []string {
	e.mu.RLock()
	sched := e.scheduler
	e.mu.RUnlock()
	if sched == nil {
		return nil
	}
	hashes := sched.TorrentHashes()
	active := hashes[:0]
	for _, hash := range hashes {
		if !sched.IsPaused(hash) {
			active = append(active, hash)
		}
	}
	sort.Strings(active)
	return active
}

// GetConfig returns a snapshot of the current configuration.
func (e *Engine) GetConfig() *config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneConfig(e.cfg)
}

func cloneConfig(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.DHTBootstrapNodes = append([]string(nil), cfg.DHTBootstrapNodes...)
	return &clone
}

func selectExplicitResumeEviction(active []string, target int) string {
	if target < 1 || len(active) < target {
		return ""
	}
	candidates := append([]string(nil), active...)
	sort.Strings(candidates)
	return candidates[len(candidates)-1]
}

func restartBoundConfigChanged(current, next *config.Config) bool {
	if current == nil || next == nil {
		return current != next
	}
	return current.Client != next.Client ||
		current.AnnounceJitterPercent != next.AnnounceJitterPercent ||
		current.PeerResponseMode != next.PeerResponseMode ||
		current.SimulateDownload != next.SimulateDownload ||
		current.RotateClientOnRestart != next.RotateClientOnRestart ||
		current.ProxyEnabled != next.ProxyEnabled ||
		current.ProxyType != next.ProxyType ||
		current.ProxyURL != next.ProxyURL ||
		current.AnnounceIP != next.AnnounceIP ||
		current.MaxAnnounceFailures != next.MaxAnnounceFailures ||
		!slices.Equal(current.DHTBootstrapNodes, next.DHTBootstrapNodes) ||
		current.EnableLabSybilRing != next.EnableLabSybilRing ||
		current.LabSybilPeers != next.LabSybilPeers ||
		current.EnablePieceProxy != next.EnablePieceProxy ||
		current.AllowPrivateNetworks != next.AllowPrivateNetworks
}

func sortedTorrents(torrents []*torrent.Torrent) []*torrent.Torrent {
	result := append([]*torrent.Torrent(nil), torrents...)
	sort.Slice(result, func(i, j int) bool { return result[i].InfoHashHex < result[j].InfoHashHex })
	return result
}

func peerWireTorrentInfo(t *torrent.Torrent, cc *announce.ClientConfig) peerwire.TorrentInfo {
	return peerwire.TorrentInfo{
		InfoHash:    t.InfoHash,
		PieceCount:  t.PieceCount,
		PeerID:      []byte(cc.PeerID),
		PieceHashes: t.PieceHashes,
		PieceLength: t.PieceLength,
		TotalSize:   t.Size,
		Metadata:    t.InfoBytes,
	}
}

func (e *Engine) registerDataFile(pw *peerwire.Server, t *torrent.Torrent) {
	if pw == nil || t == nil {
		return
	}
	torrentBase := strings.TrimSuffix(filepath.Base(t.FilePath), ".torrent")
	entries, err := os.ReadDir(e.torrentsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(strings.ToLower(entry.Name()), ".torrent") {
			continue
		}
		entryBase := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if entryBase != torrentBase {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() != t.Size {
			return
		}
		dataPath := filepath.Join(e.torrentsDir, entry.Name())
		if err := pw.RegisterDataFile(t.InfoHashHex, dataPath, t.PieceLength, t.Size, t.PieceHashes); err != nil {
			slog.Warn("data file rejected", "torrent", t.Name, "file", entry.Name(), "err", err)
		} else {
			slog.Info("SHA-1 data registered", "torrent", t.Name, "file", entry.Name(), "pieces", t.PieceCount)
		}
		return
	}
}

func (e *Engine) activateTorrent(t *torrent.Torrent, sched *announce.Scheduler, disp *bandwidth.Dispatcher, pw *peerwire.Server, dhtNode *dht.Node, cc *announce.ClientConfig) bool {
	if t == nil || sched == nil || disp == nil || cc == nil || !sched.AddTorrent(t) {
		return false
	}
	disp.RegisterTorrent(t.InfoHashHex, t.Size)
	if pw != nil {
		pw.RegisterTorrent(peerWireTorrentInfo(t, cc))
		e.registerDataFile(pw, t)
	}
	if dhtNode != nil {
		dhtNode.AddTorrent(t.InfoHashHex)
	}
	return true
}

func (e *Engine) deactivateTorrent(ctx context.Context, infoHashHex string, sched *announce.Scheduler, disp *bandwidth.Dispatcher, pw *peerwire.Server, dhtNode *dht.Node) {
	if pw != nil {
		pw.UnregisterTorrent(infoHashHex)
	}
	if dhtNode != nil {
		dhtNode.RemoveTorrent(infoHashHex)
	}
	if sched != nil {
		sched.RemoveTorrentContext(ctx, infoHashHex)
	}
	if disp != nil {
		disp.UnregisterTorrent(infoHashHex)
	}
	e.announceStatesMu.Lock()
	delete(e.announceStates, infoHashHex)
	e.announceStatesMu.Unlock()
}

func (e *Engine) torrentByHash(infoHashHex string) *torrent.Torrent {
	for _, t := range e.watcher.GetTorrents() {
		if t.InfoHashHex == infoHashHex {
			return t
		}
	}
	return nil
}

func (e *Engine) pauseTorrentRuntime(infoHashHex string) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.mu.RLock()
	seeding := e.seeding
	sched, disp, pw, dhtNode := e.scheduler, e.dispatcher, e.peerWire, e.dhtNode
	e.mu.RUnlock()
	if !seeding || sched == nil {
		return
	}
	if e.pausedSlots == nil {
		e.pausedSlots = make(map[string]bool)
	}
	e.pausedSlots[infoHashHex] = true
	if sched.HasTorrent(infoHashHex) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		e.deactivateTorrent(ctx, infoHashHex, sched, disp, pw, dhtNode)
		cancel()
	}
	e.rebalanceActiveSlotsLocked()
}

func (e *Engine) removeFailedTorrentRuntime(infoHashHex string) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.mu.RLock()
	seeding := e.seeding
	sched, disp, pw, dhtNode := e.scheduler, e.dispatcher, e.peerWire, e.dhtNode
	e.mu.RUnlock()
	if !seeding {
		return
	}
	if e.failedSlots == nil {
		e.failedSlots = make(map[string]bool)
	}
	e.failedSlots[infoHashHex] = true
	e.deactivateTorrent(context.Background(), infoHashHex, sched, disp, pw, dhtNode)
	e.rebalanceActiveSlotsLocked()
}

func (e *Engine) rebalanceActiveSlots() {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	e.rebalanceActiveSlotsLocked()
}

func (e *Engine) rebalanceActiveSlotsLocked() {
	e.mu.RLock()
	seeding := e.seeding
	sched, disp, pw, dhtNode, cc := e.scheduler, e.dispatcher, e.peerWire, e.dhtNode, e.clientConfig
	target := 0
	if e.cfg != nil {
		target = e.cfg.SimultaneousSeed
	}
	e.mu.RUnlock()
	if !seeding || sched == nil || target < 1 {
		return
	}

	all := sortedTorrents(e.watcher.GetTorrents())
	byHash := make(map[string]*torrent.Torrent, len(all))
	for _, t := range all {
		byHash[t.InfoHashHex] = t
	}
	active := sched.TorrentHashes()
	sort.Strings(active)
	for len(active) > target {
		hash := active[len(active)-1]
		active = active[:len(active)-1]
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		e.deactivateTorrent(ctx, hash, sched, disp, pw, dhtNode)
		cancel()
		if e.handlers != nil {
			e.handlers.BroadcastTorrentSlotDeactivated(hash)
		}
	}
	for _, t := range all {
		if len(active) >= target {
			break
		}
		if _, exists := byHash[t.InfoHashHex]; !exists || sched.HasTorrent(t.InfoHashHex) || e.failedSlots[t.InfoHashHex] || e.pausedSlots[t.InfoHashHex] {
			continue
		}
		if e.activateTorrent(t, sched, disp, pw, dhtNode, cc) {
			active = append(active, t.InfoHashHex)
			if e.handlers != nil {
				e.handlers.BroadcastTorrentSlotActivated(t)
			}
		}
	}
}

func removeScheduledTorrents(ctx context.Context, sched *announce.Scheduler, hashes []string) {
	workerCount := min(len(hashes), 32)
	if workerCount == 0 {
		return
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hash := range jobs {
				sched.RemoveTorrentContext(ctx, hash)
			}
		}()
	}
	for _, hash := range hashes {
		select {
		case jobs <- hash:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

// GetClientFiles returns the list of .client filenames available in the clients directory.
func (e *Engine) GetClientFiles() []string {
	entries, err := os.ReadDir(e.clientsDir)
	if err != nil {
		slog.Error("reading clients dir", "dir", e.clientsDir, "err", err)
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
		confDir         = flag.String("conf", "", "path to config directory (required)")
		port            = flag.Int("port", 5081, "web server port")
		pathPrefix      = flag.String("path-prefix", "doal", "URL path prefix")
		secretTokenFlag = flag.String("secret-token", "", "deprecated: auth token (prefer DOAL_SECRET_TOKEN)")
	)
	flag.Parse()

	if *confDir == "" {
		slog.Error("--conf is required")
		flag.Usage()
		os.Exit(1)
	}
	secretToken := strings.TrimSpace(os.Getenv("DOAL_SECRET_TOKEN"))
	if *secretTokenFlag != "" {
		if secretToken != "" {
			slog.Error("set the auth token either with DOAL_SECRET_TOKEN or --secret-token, not both")
			os.Exit(1)
		}
		secretToken = *secretTokenFlag
		slog.Warn("--secret-token exposes the token in process listings; use DOAL_SECRET_TOKEN instead")
	}
	if secretToken == "" {
		slog.Error("DOAL_SECRET_TOKEN is required (use x only for local development)")
		flag.Usage()
		os.Exit(1)
	}
	if secretToken != "x" && len(secretToken) < 32 {
		slog.Error("DOAL_SECRET_TOKEN must contain at least 32 characters")
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
	srv := web.NewServer(*port, *pathPrefix, secretToken, nil)

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
		go engine.rebalanceActiveSlots()
	}
	watcher.OnRemove = func(t *torrent.Torrent) {
		slog.Info("torrent removed", "name", t.Name, "hash", t.InfoHashHex)
		handlers.BroadcastTorrentDeleted(t)
		go func() {
			engine.slotsMu.Lock()
			defer engine.slotsMu.Unlock()
			engine.mu.RLock()
			seeding := engine.seeding
			sched, disp, pw, dhtNode := engine.scheduler, engine.dispatcher, engine.peerWire, engine.dhtNode
			engine.mu.RUnlock()
			if !seeding {
				return
			}
			delete(engine.failedSlots, t.InfoHashHex)
			delete(engine.pausedSlots, t.InfoHashHex)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			engine.deactivateTorrent(ctx, t.InfoHashHex, sched, disp, pw, dhtNode)
			cancel()
			handlers.BroadcastTorrentSlotDeactivated(t.InfoHashHex)
			engine.rebalanceActiveSlotsLocked()
		}()
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
