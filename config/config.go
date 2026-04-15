package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	SpeedModelOrganic = "ORGANIC"
	SpeedModelUniform = "UNIFORM"

	PeerResponseModeNone          = "NONE"
	PeerResponseModeHandshakeOnly = "HANDSHAKE_ONLY"
	PeerResponseModeBitfield      = "BITFIELD"
	PeerResponseModeFakeData      = "FAKE_DATA"
)

// Config holds all user-configurable settings for DOAL.
type Config struct {
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
	// EnablePortRotation is accepted but not yet implemented in the Go version.
	EnablePortRotation        bool    `json:"enablePortRotation"`
	RotateClientOnRestart     bool    `json:"rotateClientOnRestart"`
	SwarmAwareSpeed           bool    `json:"swarmAwareSpeed"`
	EnableSchedule            bool    `json:"enableSchedule"`
	ScheduleStartHour         int     `json:"scheduleStartHour"`
	ScheduleEndHour           int     `json:"scheduleEndHour"`

	// Proxy settings — empty ProxyURL means no proxy.
	ProxyEnabled bool   `json:"proxyEnabled"`
	ProxyType    string `json:"proxyType"` // "socks5" or "http"
	ProxyURL     string `json:"proxyUrl"`

	// MaxAnnounceFailures is the number of consecutive announce failures
	// before a torrent is automatically removed. 0 means unlimited.
	MaxAnnounceFailures int `json:"maxAnnounceFailures"`

	// AnnounceIP overrides the IP reported to trackers. Empty = auto.
	AnnounceIP string `json:"announceIp"`

	// path is the file this config was loaded from, not exported to JSON.
	path string
}

// Load reads and parses config.json from the given file path.
func Load(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: resolving path %q: %w", path, err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("config: reading %q: %w", absPath, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %q: %w", absPath, err)
	}

	cfg.path = absPath

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validation failed: %w", err)
	}

	return &cfg, nil
}

// Save writes the current config to the file it was loaded from.
// The caller must have loaded the config via Load before calling Save.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config: no file path set — use Load() before Save()")
	}
	return c.SaveTo(c.path)
}

// SaveTo writes the current config to the specified file path.
func (c *Config) SaveTo(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshalling: %w", err)
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("config: writing %q: %w", path, err)
	}

	return nil
}

// Validate checks that all fields are within acceptable ranges.
func (c *Config) Validate() error {
	var errs []error

	if c.MinUploadRate < 0 {
		errs = append(errs, errors.New("minUploadRate must be >= 0"))
	}
	if c.MaxUploadRate < c.MinUploadRate {
		errs = append(errs, fmt.Errorf("maxUploadRate (%d) must be >= minUploadRate (%d)", c.MaxUploadRate, c.MinUploadRate))
	}
	if c.SimultaneousSeed < 1 {
		errs = append(errs, errors.New("simultaneousSeed must be >= 1"))
	}
	if c.Client == "" {
		errs = append(errs, errors.New("client must not be empty"))
	}
	if c.SpeedModel != SpeedModelOrganic && c.SpeedModel != SpeedModelUniform {
		errs = append(errs, fmt.Errorf("speedModel must be %q or %q, got %q", SpeedModelOrganic, SpeedModelUniform, c.SpeedModel))
	}
	if c.AnnounceJitterPercent < 0 || c.AnnounceJitterPercent > 100 {
		errs = append(errs, fmt.Errorf("announceJitterPercent must be in [0, 100], got %d", c.AnnounceJitterPercent))
	}
	validPeerMode := c.PeerResponseMode == PeerResponseModeNone ||
		c.PeerResponseMode == PeerResponseModeHandshakeOnly ||
		c.PeerResponseMode == PeerResponseModeBitfield ||
		c.PeerResponseMode == PeerResponseModeFakeData
	if !validPeerMode {
		errs = append(errs, fmt.Errorf("peerResponseMode must be one of %q, %q, %q, %q, got %q",
			PeerResponseModeNone, PeerResponseModeHandshakeOnly, PeerResponseModeBitfield, PeerResponseModeFakeData,
			c.PeerResponseMode))
	}
	if c.MinSpeedWhenNoLeechers < 0 {
		errs = append(errs, errors.New("minSpeedWhenNoLeechers must be >= 0"))
	}
	if c.EnableSchedule {
		if c.ScheduleStartHour < 0 || c.ScheduleStartHour > 23 {
			errs = append(errs, fmt.Errorf("scheduleStartHour must be in [0, 23], got %d", c.ScheduleStartHour))
		}
		if c.ScheduleEndHour < 0 || c.ScheduleEndHour > 23 {
			errs = append(errs, fmt.Errorf("scheduleEndHour must be in [0, 23], got %d", c.ScheduleEndHour))
		}
		if c.ScheduleStartHour == c.ScheduleEndHour {
			errs = append(errs, fmt.Errorf("scheduleStartHour and scheduleEndHour must be different"))
		}
	}

	return errors.Join(errs...)
}

// Path returns the absolute file path this config was loaded from.
func (c *Config) Path() string {
	return c.path
}
