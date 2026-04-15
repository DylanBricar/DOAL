package config

import (
	"path/filepath"
	"testing"
)

// TestLoadAndValidate verifies that the real config.json loads and passes validation.
func TestLoadAndValidate(t *testing.T) {
	path := filepath.Join("..", "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Skipf("config.json not found (expected in CI): %v", err)
	}
	if cfg.MinUploadRate < 0 {
		t.Error("MinUploadRate should be >= 0")
	}
	if cfg.MaxUploadRate < cfg.MinUploadRate {
		t.Error("MaxUploadRate should be >= MinUploadRate")
	}
	if cfg.SimultaneousSeed < 1 {
		t.Error("SimultaneousSeed should be >= 1")
	}
	if cfg.Client == "" {
		t.Error("Client should not be empty")
	}
}

// TestLoadSetsPath verifies that the path field is populated after Load.
func TestLoadSetsPath(t *testing.T) {
	path := filepath.Join("..", "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Skipf("config.json not found (expected in CI): %v", err)
	}
	if cfg.Path() == "" {
		t.Error("Path() should be non-empty after Load")
	}
}

// TestSaveAndReload verifies that SaveTo persists a config that can be reloaded
// with identical values.
func TestSaveAndReload(t *testing.T) {
	cfg := &Config{
		MinUploadRate:    500,
		MaxUploadRate:    1000,
		SimultaneousSeed: 5,
		Client:           "test.client",
		SpeedModel:       SpeedModelOrganic,
		PeerResponseMode: PeerResponseModeBitfield,
	}
	tmp := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.SaveTo(tmp); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	loaded, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MinUploadRate != 500 {
		t.Errorf("MinUploadRate: want 500, got %d", loaded.MinUploadRate)
	}
	if loaded.MaxUploadRate != 1000 {
		t.Errorf("MaxUploadRate: want 1000, got %d", loaded.MaxUploadRate)
	}
	if loaded.SimultaneousSeed != 5 {
		t.Errorf("SimultaneousSeed: want 5, got %d", loaded.SimultaneousSeed)
	}
	if loaded.Client != "test.client" {
		t.Errorf("Client: want test.client, got %q", loaded.Client)
	}
}

// TestSaveWithoutPathErrors verifies Save() fails when config has no path.
func TestSaveWithoutPathErrors(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Save(); err == nil {
		t.Error("Save() on config with no path should return an error")
	}
}

// TestValidationErrors verifies Validate catches each invalid field individually.
func TestValidationErrors(t *testing.T) {
	base := Config{
		MinUploadRate:    100,
		MaxUploadRate:    200,
		SimultaneousSeed: 1,
		Client:           "some.client",
		SpeedModel:       SpeedModelOrganic,
		PeerResponseMode: PeerResponseModeNone,
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			"negative MinUploadRate",
			func(c *Config) { c.MinUploadRate = -1 },
		},
		{
			"MaxUploadRate less than MinUploadRate",
			func(c *Config) { c.MinUploadRate = 500; c.MaxUploadRate = 100 },
		},
		{
			"zero SimultaneousSeed",
			func(c *Config) { c.SimultaneousSeed = 0 },
		},
		{
			"empty Client",
			func(c *Config) { c.Client = "" },
		},
		{
			"invalid SpeedModel",
			func(c *Config) { c.SpeedModel = "INVALID" },
		},
		{
			"AnnounceJitterPercent below 0",
			func(c *Config) { c.AnnounceJitterPercent = -1 },
		},
		{
			"AnnounceJitterPercent above 100",
			func(c *Config) { c.AnnounceJitterPercent = 101 },
		},
		{
			"invalid PeerResponseMode",
			func(c *Config) { c.PeerResponseMode = "UNKNOWN" },
		},
		{
			"negative MinSpeedWhenNoLeechers",
			func(c *Config) { c.MinSpeedWhenNoLeechers = -1 },
		},
		{
			"ScheduleStartHour out of range when schedule enabled",
			func(c *Config) { c.EnableSchedule = true; c.ScheduleStartHour = 25 },
		},
		{
			"ScheduleEndHour out of range when schedule enabled",
			func(c *Config) { c.EnableSchedule = true; c.ScheduleEndHour = -1 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base // value copy
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("expected validation error for %q, got nil", tc.name)
			}
		})
	}
}

// TestValidAllSpeedModels verifies each speed model constant is accepted.
func TestValidAllSpeedModels(t *testing.T) {
	for _, model := range []string{SpeedModelOrganic, SpeedModelUniform} {
		cfg := Config{
			MinUploadRate:    1,
			MaxUploadRate:    2,
			SimultaneousSeed: 1,
			Client:           "x.client",
			SpeedModel:       model,
			PeerResponseMode: PeerResponseModeNone,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("SpeedModel %q should be valid, got: %v", model, err)
		}
	}
}

// TestValidAllPeerResponseModes verifies each peer mode constant is accepted.
func TestValidAllPeerResponseModes(t *testing.T) {
	modes := []string{
		PeerResponseModeNone,
		PeerResponseModeHandshakeOnly,
		PeerResponseModeBitfield,
		PeerResponseModeFakeData,
	}
	for _, mode := range modes {
		cfg := Config{
			MinUploadRate:    1,
			MaxUploadRate:    2,
			SimultaneousSeed: 1,
			Client:           "x.client",
			SpeedModel:       SpeedModelOrganic,
			PeerResponseMode: mode,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("PeerResponseMode %q should be valid, got: %v", mode, err)
		}
	}
}

// TestScheduleHoursNotValidatedWhenDisabled ensures schedule bounds are only
// checked when EnableSchedule is true.
func TestScheduleHoursNotValidatedWhenDisabled(t *testing.T) {
	cfg := Config{
		MinUploadRate:    1,
		MaxUploadRate:    2,
		SimultaneousSeed: 1,
		Client:           "x.client",
		SpeedModel:       SpeedModelOrganic,
		PeerResponseMode: PeerResponseModeNone,
		EnableSchedule:   false,
		ScheduleStartHour: 99, // would be invalid if schedule were on
		ScheduleEndHour:   -5,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("schedule hours should not be validated when EnableSchedule=false, got: %v", err)
	}
}

// TestLoadNonExistentFile verifies an error is returned for a missing path.
func TestLoadNonExistentFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.json")
	if err == nil {
		t.Error("Load on non-existent path should return an error")
	}
}
