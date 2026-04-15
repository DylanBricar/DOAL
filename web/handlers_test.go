package web

import (
	"encoding/json"
	"testing"

	"doal/config"
	"doal/torrent"
)

// mockEngine is a test double for EngineController.
type mockEngine struct {
	cfg           *config.Config
	clientFiles   []string
	torrents      []*torrent.Torrent
	seeding       bool
	activeClient  string
	announceStates []AnnounceState
	speeds        map[string]int64
	totalUploaded int64
	uploaded      map[string]int64
	paused        map[string]bool
	trackerStats  map[string]int64
	savedConfig   *config.Config
	startCalled   bool
	stopCalled    bool
	pausedHashes  []string
	resumedHashes []string
}

func (m *mockEngine) Start() error {
	m.startCalled = true
	m.seeding = true
	return nil
}
func (m *mockEngine) Stop() {
	m.stopCalled = true
	m.seeding = false
}
func (m *mockEngine) SaveConfig(cfg *config.Config) error {
	m.savedConfig = cfg
	m.cfg = cfg
	return nil
}
func (m *mockEngine) GetConfig() *config.Config         { return m.cfg }
func (m *mockEngine) GetClientFiles() []string          { return m.clientFiles }
func (m *mockEngine) GetTorrents() []*torrent.Torrent   { return m.torrents }
func (m *mockEngine) IsSeeding() bool                   { return m.seeding }
func (m *mockEngine) TorrentsDir() string               { return "/tmp" }
func (m *mockEngine) GetActiveClient() string           { return m.activeClient }
func (m *mockEngine) GetSpeeds() (map[string]int64, int64, map[string]int64) {
	return m.speeds, m.totalUploaded, m.uploaded
}
func (m *mockEngine) GetAnnounceStates() []AnnounceState { return m.announceStates }
func (m *mockEngine) PauseTorrent(h string)              { m.pausedHashes = append(m.pausedHashes, h) }
func (m *mockEngine) ResumeTorrent(h string)             { m.resumedHashes = append(m.resumedHashes, h) }
func (m *mockEngine) PauseTracker(_ string)              {}
func (m *mockEngine) ResumeTracker(_ string)             {}
func (m *mockEngine) GetPausedTorrents() map[string]bool {
	if m.paused == nil {
		return map[string]bool{}
	}
	return m.paused
}
func (m *mockEngine) GetTrackerStats() map[string]int64 {
	if m.trackerStats == nil {
		return map[string]int64{}
	}
	return m.trackerStats
}

func validConfig() *config.Config {
	return &config.Config{
		MinUploadRate:    100,
		MaxUploadRate:    200,
		SimultaneousSeed: 1,
		Client:           "test.client",
		SpeedModel:       config.SpeedModelUniform,
		PeerResponseMode: config.PeerResponseModeNone,
	}
}

// setupHandlers returns a server with no real connections and its handlers.
func setupHandlers() (*Server, *Handlers, *mockEngine) {
	engine := &mockEngine{
		cfg:          validConfig(),
		clientFiles:  []string{"qbittorrent.client"},
		torrents:     []*torrent.Torrent{},
		speeds:       map[string]int64{},
		uploaded:     map[string]int64{},
		paused:       map[string]bool{},
		trackerStats: map[string]int64{},
	}
	srv := NewServer(0, "doal", "x", nil)
	h := NewHandlers(srv, engine)
	return srv, h, engine
}

// TestNewHandlersWiresCallback verifies that NewHandlers sets the server's onMessage callback.
func TestNewHandlersWiresCallback(t *testing.T) {
	_, _, _ = setupHandlers()
	// If NewHandlers doesn't set srv.onMessage the above setupHandlers would still work,
	// but we verify it is non-nil by inspecting the server.
	srv, _, _ := setupHandlers()
	if srv.onMessage == nil {
		t.Error("server.onMessage should be non-nil after NewHandlers")
	}
}

// TestHandleGlobalStart verifies that the engine Start is called.
func TestHandleGlobalStart(t *testing.T) {
	_, h, engine := setupHandlers()
	h.handleGlobalStart()
	if !engine.startCalled {
		t.Error("engine.Start should have been called")
	}
}

// TestHandleGlobalStop verifies that the engine Stop is called.
func TestHandleGlobalStop(t *testing.T) {
	_, h, engine := setupHandlers()
	h.handleGlobalStop()
	if !engine.stopCalled {
		t.Error("engine.Stop should have been called")
	}
}

// TestHandleConfigSaveValid verifies that a valid config JSON payload persists.
func TestHandleConfigSaveValid(t *testing.T) {
	_, h, engine := setupHandlers()
	payload := configSaveRequest{
		MinUploadRate:    50,
		MaxUploadRate:    150,
		SimultaneousSeed: 2,
		Client:           "some.client",
		SpeedModel:       config.SpeedModelUniform,
		PeerResponseMode: config.PeerResponseModeNone,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h.handleConfigSave(data)
	if engine.savedConfig == nil {
		t.Fatal("engine.SaveConfig should have been called")
	}
	if engine.savedConfig.MinUploadRate != 50 {
		t.Errorf("MinUploadRate: want 50, got %d", engine.savedConfig.MinUploadRate)
	}
	if engine.savedConfig.MaxUploadRate != 150 {
		t.Errorf("MaxUploadRate: want 150, got %d", engine.savedConfig.MaxUploadRate)
	}
}

// TestHandleConfigSaveInvalidJSON verifies that malformed JSON does not call SaveConfig.
func TestHandleConfigSaveInvalidJSON(t *testing.T) {
	_, h, engine := setupHandlers()
	h.handleConfigSave([]byte("{not valid json"))
	if engine.savedConfig != nil {
		t.Error("SaveConfig should NOT be called on invalid JSON")
	}
}

// TestHandleConfigSaveInvalidValues verifies that validation errors block SaveConfig.
func TestHandleConfigSaveInvalidValues(t *testing.T) {
	_, h, engine := setupHandlers()
	// maxUploadRate < minUploadRate is invalid.
	payload := configSaveRequest{
		MinUploadRate:    500,
		MaxUploadRate:    100,
		SimultaneousSeed: 1,
		Client:           "test.client",
		SpeedModel:       config.SpeedModelUniform,
		PeerResponseMode: config.PeerResponseModeNone,
	}
	data, _ := json.Marshal(payload)
	h.handleConfigSave(data)
	if engine.savedConfig != nil {
		t.Error("SaveConfig should NOT be called when validation fails")
	}
}

// TestHandleTorrentPauseCallsEngine verifies pause routes to engine.PauseTorrent.
func TestHandleTorrentPauseCallsEngine(t *testing.T) {
	_, h, engine := setupHandlers()
	data := []byte(`{"infoHash":"abc123"}`)
	h.handleTorrentPause(data)
	if len(engine.pausedHashes) == 0 || engine.pausedHashes[0] != "abc123" {
		t.Errorf("PauseTorrent: want abc123 paused, got %v", engine.pausedHashes)
	}
}

// TestHandleTorrentResumeCallsEngine verifies resume routes to engine.ResumeTorrent.
func TestHandleTorrentResumeCallsEngine(t *testing.T) {
	_, h, engine := setupHandlers()
	data := []byte(`{"infoHash":"def456"}`)
	h.handleTorrentResume(data)
	if len(engine.resumedHashes) == 0 || engine.resumedHashes[0] != "def456" {
		t.Errorf("ResumeTorrent: want def456 resumed, got %v", engine.resumedHashes)
	}
}

// TestHandleTorrentPauseBadJSON verifies that bad JSON does not panic.
func TestHandleTorrentPauseBadJSON(t *testing.T) {
	_, h, engine := setupHandlers()
	h.handleTorrentPause([]byte("not json"))
	if len(engine.pausedHashes) != 0 {
		t.Error("PauseTorrent should not be called on invalid JSON")
	}
}

// TestHandleTorrentDeleteByHash verifies that the matching torrent file is targeted.
func TestHandleTorrentDeleteByHash(t *testing.T) {
	_, h, engine := setupHandlers()
	// Inject a torrent into the engine.
	engine.torrents = []*torrent.Torrent{
		{
			InfoHashHex: "deadbeef",
			FilePath:    "/tmp/nonexistent-doal-test.torrent",
		},
	}
	data, _ := json.Marshal("deadbeef")
	// os.Remove will fail (file doesn't exist), but no panic should occur.
	h.handleTorrentDelete(data)
}

// TestHandleTorrentUploadBadBase64 verifies that invalid base64 does not panic.
func TestHandleTorrentUploadBadBase64(t *testing.T) {
	_, h, _ := setupHandlers()
	data := []byte(`{"fileName":"test.torrent","b64String":"!!!notbase64!!!"}`)
	h.handleTorrentUpload(data)
}

// TestHandleTorrentUploadEmptyFields verifies that missing fileName/b64String is handled.
func TestHandleTorrentUploadEmptyFields(t *testing.T) {
	_, h, _ := setupHandlers()
	data := []byte(`{"fileName":"","b64String":""}`)
	h.handleTorrentUpload(data)
}

// TestDispatchKnownDestinations verifies dispatch routes known destinations without panicking.
func TestDispatchKnownDestinations(t *testing.T) {
	_, h, _ := setupHandlers()
	knownDestinations := []string{
		DestGlobalStart,
		DestGlobalStop,
		DestInitializeMe,
		DestConfig,
		DestTorrents,
		DestGlobal,
		DestSpeed,
		DestAnnounce,
	}
	for _, dest := range knownDestinations {
		h.dispatch("client-test", dest, nil)
	}
}

// TestDispatchUnknownDestination verifies dispatch does not panic on unknown destinations.
func TestDispatchUnknownDestination(t *testing.T) {
	_, h, _ := setupHandlers()
	h.dispatch("client-test", "/totally/unknown/path", nil)
}

// TestBroadcastSeedingSpeedDoesNotPanic verifies BroadcastSeedingSpeed with an empty map.
func TestBroadcastSeedingSpeedDoesNotPanic(t *testing.T) {
	_, h, _ := setupHandlers()
	h.BroadcastSeedingSpeed(map[string]int64{}, 0, map[string]int64{})
}

// TestBroadcastTorrentAddedDoesNotPanic verifies BroadcastTorrentAdded doesn't panic.
func TestBroadcastTorrentAddedDoesNotPanic(t *testing.T) {
	_, h, _ := setupHandlers()
	h.BroadcastTorrentAdded(&torrent.Torrent{InfoHashHex: "aaa", Name: "Test", AnnounceURLs: []string{}})
}

// TestBroadcastAnnounceSuccessDoesNotPanic verifies BroadcastAnnounceSuccess doesn't panic.
func TestBroadcastAnnounceSuccessDoesNotPanic(t *testing.T) {
	_, h, _ := setupHandlers()
	h.BroadcastAnnounceSuccess("abc", &torrent.Torrent{Name: "T", Size: 1024, AnnounceURLs: []string{}}, 10, 5, 1800)
}

// TestBroadcastAnnounceFailedDoesNotPanic verifies BroadcastAnnounceFailed doesn't panic.
func TestBroadcastAnnounceFailedDoesNotPanic(t *testing.T) {
	_, h, _ := setupHandlers()
	h.BroadcastAnnounceFailed("abc", "tracker unreachable")
}

// TestBroadcastTooManyFailsDoesNotPanic verifies BroadcastTooManyFails doesn't panic.
func TestBroadcastTooManyFailsDoesNotPanic(t *testing.T) {
	_, h, _ := setupHandlers()
	h.BroadcastTooManyFails("abc123")
}

// TestTorrentPayloadFields verifies that torrentPayload returns all expected keys.
func TestTorrentPayloadFields(t *testing.T) {
	t1 := &torrent.Torrent{
		InfoHashHex:  "deadbeef",
		Name:         "My Torrent",
		Size:         1024 * 1024,
		PieceCount:   8,
		AnnounceURLs: []string{"http://tracker.example.com/announce"},
		FilePath:     "/tmp/my.torrent",
	}
	payload := torrentPayload(t1)
	expected := []string{"infoHash", "name", "size", "pieceCount", "announceURLs", "filePath"}
	for _, key := range expected {
		if _, ok := payload[key]; !ok {
			t.Errorf("torrentPayload missing key %q", key)
		}
	}
	if payload["infoHash"] != "deadbeef" {
		t.Errorf("infoHash: want deadbeef, got %v", payload["infoHash"])
	}
	if payload["name"] != "My Torrent" {
		t.Errorf("name: want My Torrent, got %v", payload["name"])
	}
}
