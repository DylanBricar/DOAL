package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"time"

	"doal/config"
	"doal/torrent"
)

// EngineController is the interface the web layer uses to drive the core engine.
// The concrete Engine type in main.go implements this interface.
type EngineController interface {
	Start() error
	Stop()
	SaveConfig(cfg *config.Config) error
	GetConfig() *config.Config
	GetClientFiles() []string
	GetTorrents() []*torrent.Torrent
	IsSeeding() bool
	TorrentsDir() string
	GetActiveClient() string
	GetSpeeds() (speeds map[string]int64, totalUploaded int64, uploaded map[string]int64)
	GetAnnounceStates() []AnnounceState
	PauseTorrent(infoHashHex string)
	ResumeTorrent(infoHashHex string)
	PauseTracker(domain string)
	ResumeTracker(domain string)
	GetPausedTorrents() map[string]bool
	GetTrackerStats() map[string]int64
}

// AnnounceState holds per-torrent announce info for init replay.
type AnnounceState struct {
	InfoHashHex string
	Name        string
	Size        int64
	Seeders     int
	Leechers    int
	Interval    int
	AnnouncedAt string
	Uploaded    int64
}

// StompMessage is the envelope for all outgoing WebSocket messages.
type StompMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// Outgoing message type constants.
const (
	MsgConfigHasBeenLoaded    = "CONFIG_HAS_BEEN_LOADED"
	MsgListOfClientFiles      = "LIST_OF_CLIENT_FILES"
	MsgGlobalSeedStarted      = "GLOBAL_SEED_STARTED"
	MsgGlobalSeedStopped      = "GLOBAL_SEED_STOPPED"
	MsgTorrentFileAdded       = "TORRENT_FILE_ADDED"
	MsgTorrentFileDeleted     = "TORRENT_FILE_DELETED"
	MsgSuccessfullyAnnounce   = "SUCCESSFULLY_ANNOUNCE"
	MsgWillAnnounce           = "WILL_ANNOUNCE"
	MsgFailedToAnnounce       = "FAILED_TO_ANNOUNCE"
	MsgTooManyAnnouncesFailed = "TOO_MANY_ANNOUNCES_FAILED"
	MsgSeedingSpeedHasChanged = "SEEDING_SPEED_HAS_CHANGED"
	MsgInvalidConfig          = "INVALID_CONFIG"
	MsgTrackerStats           = "TRACKER_STATS"
)

// Incoming STOMP destination constants.
const (
	DestGlobalStart       = "/doal/global/start"
	DestGlobalStop        = "/doal/global/stop"
	DestConfigSave        = "/doal/config/save"
	DestTorrentsUpload    = "/doal/torrents/upload"
	DestTorrentsDelete    = "/doal/torrents/delete"
	DestTorrentsPause     = "/doal/torrents/pause"
	DestTorrentsResume    = "/doal/torrents/resume"
	DestTrackerPause      = "/doal/tracker/pause"
	DestTrackerResume     = "/doal/tracker/resume"
	DestInitializeMe      = "/doal/initialize-me"
	DestConfig            = "/config"
	DestTorrents          = "/torrents"
	DestGlobal            = "/global"
	DestSpeed             = "/speed"
	DestAnnounce          = "/announce"
)

// Handlers bridges the web layer to the core engine.
type Handlers struct {
	server *Server
	engine EngineController
}

// NewHandlers constructs a Handlers and wires it to the server's onMessage callback.
func NewHandlers(server *Server, engine EngineController) *Handlers {
	h := &Handlers{
		server: server,
		engine: engine,
	}
	server.onMessage = h.dispatch
	return h
}

// dispatch routes an incoming STOMP message to the correct handler.
// It is called for both SEND frames (data != nil) and SUBSCRIBE frames (data == nil).
func (h *Handlers) dispatch(clientID string, destination string, data []byte) {
	fmt.Printf("dispatch: client=%s dest=%q len=%d\n", clientID, destination, len(data))
	switch destination {
	case DestInitializeMe:
		h.handleInitializeMe(clientID)
	case DestGlobalStart:
		h.handleGlobalStart()
	case DestGlobalStop:
		h.handleGlobalStop()
	case DestConfigSave:
		h.handleConfigSave(data)
	case DestTorrentsUpload:
		h.handleTorrentUpload(data)
	case DestTorrentsDelete:
		h.handleTorrentDelete(data)
	case DestTorrentsPause:
		h.handleTorrentPause(data)
	case DestTorrentsResume:
		h.handleTorrentResume(data)
	case DestTrackerPause:
		h.handleTrackerPause(data)
	case DestTrackerResume:
		h.handleTrackerResume(data)
	case "/global", "/announce", "/config", "/torrents", "/speed":
		// SUBSCRIBE destinations — handled by STOMP layer, nothing to do here
	default:
		fmt.Printf("handlers: unhandled destination %q from client %s\n", destination, clientID)
	}
}

// handleInitializeMe sends the current full state to the newly subscribing client.
func (h *Handlers) handleInitializeMe(clientID string) {
	cfg := h.engine.GetConfig()
	clientFiles := h.engine.GetClientFiles()
	torrents := h.engine.GetTorrents()
	seeding := h.engine.IsSeeding()

	h.server.SendToAll(DestConfig, StompMessage{
		Type:    MsgConfigHasBeenLoaded,
		Payload: map[string]interface{}{"config": cfg},
	})

	h.server.SendToAll(DestConfig, StompMessage{
		Type:    MsgListOfClientFiles,
		Payload: map[string]interface{}{"clients": clientFiles},
	})

	for _, t := range torrents {
		h.server.SendToAll(DestTorrents, StompMessage{
			Type:    MsgTorrentFileAdded,
			Payload: torrentPayload(t),
		})
	}

	if seeding {
		h.server.SendToAll(DestGlobal, StompMessage{
			Type:    MsgGlobalSeedStarted,
			Payload: map[string]interface{}{"client": h.engine.GetActiveClient()},
		})

		// Replay active announce states so the UI shows "Torrents en Seed"
		for _, a := range h.engine.GetAnnounceStates() {
			h.server.SendToAll(DestAnnounce, StompMessage{
				Type: MsgSuccessfullyAnnounce,
				Payload: map[string]interface{}{
					"infoHash":          a.InfoHashHex,
					"torrentName":       a.Name,
					"torrentSize":       a.Size,
					"lastKnownSeeders":  a.Seeders,
					"lastKnownLeechers": a.Leechers,
					"lastKnownInterval": a.Interval,
					"lastAnnouncedAt":   a.AnnouncedAt,
					"requestEvent":      "STARTED",
					"uploaded":          a.Uploaded,
				},
			})
		}

		// Replay paused state for each torrent
		paused := h.engine.GetPausedTorrents()
		for hash, isPaused := range paused {
			if isPaused {
				h.server.SendToAll(DestAnnounce, StompMessage{
					Type:    "TORRENT_PAUSED",
					Payload: map[string]interface{}{"infoHash": hash},
				})
			}
		}

		// Replay current speeds
		speeds, totalUploaded, uploaded := h.engine.GetSpeeds()
		h.BroadcastSeedingSpeed(speeds, totalUploaded, uploaded)

		// Send tracker stats
		trackerStats := h.engine.GetTrackerStats()
		h.BroadcastTrackerStats(trackerStats)
	} else {
		h.server.SendToAll(DestGlobal, StompMessage{
			Type:    MsgGlobalSeedStopped,
			Payload: map[string]interface{}{},
		})
	}

	fmt.Printf("handlers: initialized client %s\n", clientID)
}

func (h *Handlers) handleGlobalStart() {
	if err := h.engine.Start(); err != nil {
		fmt.Printf("handlers: engine start: %v\n", err)
		return
	}
	h.server.SendToAll(DestGlobal, StompMessage{
		Type:    MsgGlobalSeedStarted,
		Payload: map[string]interface{}{"client": h.engine.GetConfig().Client},
	})
}

func (h *Handlers) handleGlobalStop() {
	h.engine.Stop()
	h.server.SendToAll(DestGlobal, StompMessage{
		Type:    MsgGlobalSeedStopped,
		Payload: map[string]interface{}{},
	})
}

// configSaveRequest is the JSON body sent to /doal/config/save.
type configSaveRequest struct {
	MinUploadRate             int64   `json:"minUploadRate"`
	MaxUploadRate             int64   `json:"maxUploadRate"`
	SimultaneousSeed          int     `json:"simultaneousSeed"`
	Client                    string  `json:"client"`
	KeepTorrentWithZeroLeechers bool  `json:"keepTorrentWithZeroLeechers"`
	UploadRatioTarget         float64 `json:"uploadRatioTarget"`
	SpeedModel                string  `json:"speedModel"`
	AnnounceJitterPercent     int     `json:"announceJitterPercent"`
	PeerResponseMode          string  `json:"peerResponseMode"`
	PerTorrentBandwidth       bool    `json:"perTorrentBandwidth"`
	MinSpeedWhenNoLeechers    int64   `json:"minSpeedWhenNoLeechers"`
	SimulateDownload          bool    `json:"simulateDownload"`
	EnableBurstSpeed          bool    `json:"enableBurstSpeed"`
	EnablePortRotation        bool    `json:"enablePortRotation"`
	RotateClientOnRestart     bool    `json:"rotateClientOnRestart"`
	SwarmAwareSpeed           bool    `json:"swarmAwareSpeed"`
	EnableSchedule            bool    `json:"enableSchedule"`
	ScheduleStartHour         int     `json:"scheduleStartHour"`
	ScheduleEndHour           int     `json:"scheduleEndHour"`
	ProxyEnabled              bool    `json:"proxyEnabled"`
	ProxyType                 string  `json:"proxyType"`
	ProxyURL                  string  `json:"proxyUrl"`
	AnnounceIP                string  `json:"announceIp"`
	MaxAnnounceFailures       int     `json:"maxAnnounceFailures"`
}

func (h *Handlers) handleConfigSave(data []byte) {
	var req configSaveRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Printf("handlers: parsing config save request: %v\n", err)
		h.server.SendToAll(DestConfig, StompMessage{
			Type:    MsgInvalidConfig,
			Payload: err.Error(),
		})
		return
	}

	cfg := &config.Config{
		MinUploadRate:             req.MinUploadRate,
		MaxUploadRate:             req.MaxUploadRate,
		SimultaneousSeed:          req.SimultaneousSeed,
		Client:                    req.Client,
		KeepTorrentWithZeroLeechers: req.KeepTorrentWithZeroLeechers,
		UploadRatioTarget:         req.UploadRatioTarget,
		SpeedModel:                req.SpeedModel,
		AnnounceJitterPercent:     req.AnnounceJitterPercent,
		PeerResponseMode:          req.PeerResponseMode,
		PerTorrentBandwidth:       req.PerTorrentBandwidth,
		MinSpeedWhenNoLeechers:    req.MinSpeedWhenNoLeechers,
		SimulateDownload:          req.SimulateDownload,
		EnableBurstSpeed:          req.EnableBurstSpeed,
		EnablePortRotation:        req.EnablePortRotation,
		RotateClientOnRestart:     req.RotateClientOnRestart,
		SwarmAwareSpeed:           req.SwarmAwareSpeed,
		EnableSchedule:            req.EnableSchedule,
		ScheduleStartHour:         req.ScheduleStartHour,
		ScheduleEndHour:           req.ScheduleEndHour,
		ProxyEnabled:              req.ProxyEnabled,
		ProxyType:                 req.ProxyType,
		ProxyURL:                  req.ProxyURL,
		AnnounceIP:                req.AnnounceIP,
		MaxAnnounceFailures:       req.MaxAnnounceFailures,
	}

	if err := cfg.Validate(); err != nil {
		fmt.Printf("handlers: invalid config: %v\n", err)
		h.server.SendToAll(DestConfig, StompMessage{
			Type:    MsgInvalidConfig,
			Payload: err.Error(),
		})
		return
	}

	if err := h.engine.SaveConfig(cfg); err != nil {
		fmt.Printf("handlers: saving config: %v\n", err)
		return
	}

	h.server.SendToAll(DestConfig, StompMessage{
		Type:    MsgConfigHasBeenLoaded,
		Payload: cfg,
	})
}

// torrentUploadRequest is the JSON body sent to /doal/torrents/upload.
type torrentUploadRequest struct {
	FileName  string `json:"fileName"`
	B64String string `json:"b64String"`
}

func (h *Handlers) handleTorrentUpload(data []byte) {
	var req torrentUploadRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Printf("handlers: parsing torrent upload request: %v\n", err)
		return
	}

	if req.FileName == "" || req.B64String == "" {
		fmt.Println("handlers: torrent upload missing fileName or base64")
		return
	}

	// Strip data-URI prefix if present.
	b64 := req.B64String
	if idx := strings.Index(b64, ","); idx >= 0 {
		b64 = b64[idx+1:]
	}

	torrentBytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		fmt.Printf("handlers: decoding base64 torrent %q: %v\n", req.FileName, err)
		return
	}

	safeName := filepath.Base(req.FileName)
	if !strings.HasSuffix(strings.ToLower(safeName), ".torrent") {
		safeName += ".torrent"
	}

	destPath := filepath.Join(h.engine.TorrentsDir(), safeName)
	if !strings.HasPrefix(filepath.Clean(destPath), filepath.Clean(h.engine.TorrentsDir())) {
		fmt.Printf("handlers: path traversal attempt: %q\n", destPath)
		return
	}
	if err := os.WriteFile(destPath, torrentBytes, 0o644); err != nil {
		fmt.Printf("handlers: writing torrent file %q: %v\n", destPath, err)
		return
	}

	fmt.Printf("handlers: torrent uploaded: %s\n", destPath)
	// The watcher will pick up the new file and fire TORRENT_FILE_ADDED.
}

func (h *Handlers) handleTorrentDelete(data []byte) {
	// The JS sends JSON.stringify(hash) which is a JSON string like "abc123..."
	// Or it could be a raw string. Try to unmarshal as JSON string first.
	var infoHash string
	if err := json.Unmarshal(data, &infoHash); err != nil {
		// Fallback: use raw data as the hash
		infoHash = strings.TrimSpace(string(data))
	}

	if infoHash == "" {
		fmt.Println("handlers: torrent delete: empty hash")
		return
	}

	// Find the torrent file matching this infoHash
	torrents := h.engine.GetTorrents()
	for _, t := range torrents {
		if t.InfoHashHex == infoHash {
			if err := os.Remove(t.FilePath); err != nil {
				fmt.Printf("handlers: deleting torrent %q: %v\n", t.FilePath, err)
				return
			}
			fmt.Printf("handlers: torrent deleted: %s\n", t.FilePath)
			return
		}
	}
	fmt.Printf("handlers: torrent not found for hash %q\n", infoHash)
}

// infoHashRequest is the JSON body for single-torrent pause/resume.
type infoHashRequest struct {
	InfoHash string `json:"infoHash"`
}

// trackerDomainRequest is the JSON body for tracker-level pause/resume.
type trackerDomainRequest struct {
	Tracker string `json:"tracker"`
}

func (h *Handlers) handleTorrentPause(data []byte) {
	var req infoHashRequest
	if err := json.Unmarshal(data, &req); err != nil || req.InfoHash == "" {
		fmt.Printf("handlers: torrent pause: bad request: %v\n", err)
		return
	}
	h.engine.PauseTorrent(req.InfoHash)
	h.server.SendToAll(DestAnnounce, StompMessage{Type: "TORRENT_PAUSED", Payload: map[string]interface{}{"infoHash": req.InfoHash}})
	fmt.Printf("handlers: torrent paused: %s\n", req.InfoHash)
}

func (h *Handlers) handleTorrentResume(data []byte) {
	var req infoHashRequest
	if err := json.Unmarshal(data, &req); err != nil || req.InfoHash == "" {
		fmt.Printf("handlers: torrent resume: bad request: %v\n", err)
		return
	}
	h.engine.ResumeTorrent(req.InfoHash)
	h.server.SendToAll(DestAnnounce, StompMessage{Type: "TORRENT_RESUMED", Payload: map[string]interface{}{"infoHash": req.InfoHash}})
	fmt.Printf("handlers: torrent resumed: %s\n", req.InfoHash)
}

func (h *Handlers) handleTrackerPause(data []byte) {
	var req trackerDomainRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Tracker == "" {
		fmt.Printf("handlers: tracker pause: bad request: %v\n", err)
		return
	}
	h.engine.PauseTracker(req.Tracker)
	for hash, paused := range h.engine.GetPausedTorrents() {
		if paused {
			h.server.SendToAll(DestAnnounce, StompMessage{Type: "TORRENT_PAUSED", Payload: map[string]interface{}{"infoHash": hash}})
		}
	}
	h.BroadcastTrackerStats(h.engine.GetTrackerStats())
	fmt.Printf("handlers: tracker paused: %s\n", req.Tracker)
}

func (h *Handlers) handleTrackerResume(data []byte) {
	var req trackerDomainRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Tracker == "" {
		fmt.Printf("handlers: tracker resume: bad request: %v\n", err)
		return
	}
	h.engine.ResumeTracker(req.Tracker)
	for _, t := range h.engine.GetTorrents() {
		for _, u := range t.AnnounceURLs {
			if strings.Contains(u, req.Tracker) {
				h.server.SendToAll(DestAnnounce, StompMessage{Type: "TORRENT_RESUMED", Payload: map[string]interface{}{"infoHash": t.InfoHashHex}})
				break
			}
		}
	}
	h.BroadcastTrackerStats(h.engine.GetTrackerStats())
	fmt.Printf("handlers: tracker resumed: %s\n", req.Tracker)
}

// Broadcast helpers — called by the engine to push events to all clients.

// BroadcastTorrentAdded notifies all clients that a torrent was added.
func (h *Handlers) BroadcastTorrentAdded(t *torrent.Torrent) {
	h.server.SendToAll(DestTorrents, StompMessage{
		Type:    MsgTorrentFileAdded,
		Payload: torrentPayload(t),
	})
}

// BroadcastTorrentDeleted notifies all clients that a torrent was removed.
func (h *Handlers) BroadcastTorrentDeleted(t *torrent.Torrent) {
	h.server.SendToAll(DestTorrents, StompMessage{
		Type:    MsgTorrentFileDeleted,
		Payload: torrentPayload(t),
	})
}

// BroadcastAnnounceSuccess notifies all clients of a successful announce.
func (h *Handlers) BroadcastAnnounceSuccess(infoHashHex string, t *torrent.Torrent, seeders, leechers, interval int) {
	h.server.SendToAll(DestAnnounce, StompMessage{
		Type: MsgSuccessfullyAnnounce,
		Payload: map[string]interface{}{
			"infoHash":          infoHashHex,
			"torrentName":       t.Name,
			"torrentSize":       t.Size,
			"lastKnownSeeders":  seeders,
			"lastKnownLeechers": leechers,
			"lastKnownInterval": interval,
			"lastAnnouncedAt":   time.Now().Format(time.RFC3339),
			"requestEvent":      "STARTED",
		},
	})
}

// BroadcastTooManyFails notifies all clients that a torrent was removed due to
// too many consecutive announce failures.
func (h *Handlers) BroadcastTooManyFails(infoHashHex string) {
	h.server.SendToAll(DestAnnounce, StompMessage{
		Type:    MsgTooManyAnnouncesFailed,
		Payload: map[string]interface{}{"infoHash": infoHashHex},
	})
}

// BroadcastAnnounceFailed notifies all clients of a failed announce.
func (h *Handlers) BroadcastAnnounceFailed(infoHashHex string, errMsg string) {
	h.server.SendToAll(DestAnnounce, StompMessage{
		Type: MsgFailedToAnnounce,
		Payload: map[string]interface{}{
			"infoHash":   infoHashHex,
			"errMessage": errMsg,
		},
	})
}

// BroadcastSeedingSpeed notifies all clients of the current seeding speed.
func (h *Handlers) BroadcastSeedingSpeed(speeds map[string]int64, totalUploaded int64, uploaded map[string]int64) {
	type speedEntry struct {
		InfoHash       string `json:"infoHash"`
		BytesPerSecond int64  `json:"bytesPerSecond"`
		Uploaded       int64  `json:"uploaded"`
	}
	entries := make([]speedEntry, 0, len(speeds))
	for hash, bps := range speeds {
		entries = append(entries, speedEntry{InfoHash: hash, BytesPerSecond: bps, Uploaded: uploaded[hash]})
	}
	h.server.SendToAll(DestSpeed, StompMessage{
		Type: MsgSeedingSpeedHasChanged,
		Payload: map[string]interface{}{
			"speeds":        entries,
			"totalUploaded": totalUploaded,
		},
	})
}

// BroadcastTrackerStats sends aggregated per-tracker upload stats to all clients.
func (h *Handlers) BroadcastTrackerStats(stats map[string]int64) {
	type trackerEntry struct {
		Domain        string `json:"domain"`
		TotalUploaded int64  `json:"totalUploaded"`
		TorrentCount  int    `json:"torrentCount"`
		Paused        bool   `json:"paused"`
	}
	// Count torrents per tracker and check paused state
	trackerTorrents := make(map[string]int)
	trackerPaused := make(map[string]bool)
	pausedMap := h.engine.GetPausedTorrents()
	for _, t := range h.engine.GetTorrents() {
		if len(t.AnnounceURLs) > 0 {
			u := t.AnnounceURLs[0]
			domain := u
			if idx := strings.Index(u, "://"); idx >= 0 { domain = u[idx+3:] }
			if idx := strings.Index(domain, "/"); idx >= 0 { domain = domain[:idx] }
			trackerTorrents[domain]++
			if !pausedMap[t.InfoHashHex] {
				trackerPaused[domain] = false // at least one active
			}
		}
	}
	// If all torrents of a tracker are paused, mark tracker as paused
	for domain := range trackerTorrents {
		if _, hasActive := trackerPaused[domain]; !hasActive {
			trackerPaused[domain] = true
		}
	}

	entries := make([]trackerEntry, 0, len(stats))
	for domain, total := range stats {
		entries = append(entries, trackerEntry{
			Domain: domain, TotalUploaded: total,
			TorrentCount: trackerTorrents[domain],
			Paused: trackerPaused[domain],
		})
	}
	h.server.SendToAll(DestSpeed, StompMessage{
		Type:    MsgTrackerStats,
		Payload: map[string]interface{}{"trackers": entries},
	})
}

// torrentPayload converts a Torrent to a map suitable for JSON serialization.
func torrentPayload(t *torrent.Torrent) map[string]interface{} {
	return map[string]interface{}{
		"infoHash":     t.InfoHashHex,
		"name":         t.Name,
		"size":         t.Size,
		"pieceCount":   t.PieceCount,
		"announceURLs": t.AnnounceURLs,
		"filePath":     t.FilePath,
	}
}
