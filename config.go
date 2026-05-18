package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// BitratePreset is one of "low" | "medium" | "high". Used for the hardware
// encoder. Software encoding uses CRF and ignores this.
type BitratePreset string

const (
	BitrateLow    BitratePreset = "low"
	BitrateMedium BitratePreset = "medium"
	BitrateHigh   BitratePreset = "high"
)

func (b BitratePreset) FFArg() string {
	switch b {
	case BitrateLow:
		return "2M"
	case BitrateHigh:
		return "8M"
	default:
		return "4M"
	}
}

func ValidBitrate(s string) BitratePreset {
	switch BitratePreset(s) {
	case BitrateLow, BitrateMedium, BitrateHigh:
		return BitratePreset(s)
	default:
		return BitrateMedium
	}
}

type Config struct {
	DataDir         string        `json:"data_dir"`
	Camera          string        `json:"camera"`
	HardwareEncode  bool          `json:"hardware_encode"`
	HardwareBitrate BitratePreset `json:"hardware_bitrate"`

	// Runtime-only: set by the startup probe, not persisted.
	hwAvailable bool

	path string
	mu   sync.RWMutex
}

func LoadConfig(path, defaultDataDir, defaultCamera string) (*Config, error) {
	c := &Config{
		DataDir:         defaultDataDir,
		Camera:          defaultCamera,
		HardwareEncode:  false,
		HardwareBitrate: BitrateMedium,
		path:            path,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, c.Save()
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	if c.Camera == "" {
		c.Camera = defaultCamera
	}
	c.HardwareBitrate = ValidBitrate(string(c.HardwareBitrate))
	return c, nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		DataDir         string        `json:"data_dir"`
		Camera          string        `json:"camera"`
		HardwareEncode  bool          `json:"hardware_encode"`
		HardwareBitrate BitratePreset `json:"hardware_bitrate"`
	}{c.DataDir, c.Camera, c.HardwareEncode, c.HardwareBitrate}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}

func (c *Config) Snapshot() (dataDir, camera string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DataDir, c.Camera
}

func (c *Config) EncodeSettings() (hwEnabled, hwAvailable bool, bitrate BitratePreset) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.HardwareEncode, c.hwAvailable, c.HardwareBitrate
}

func (c *Config) HardwareAvailable() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.hwAvailable
}

func (c *Config) SetHardwareAvailable(v bool) {
	c.mu.Lock()
	c.hwAvailable = v
	c.mu.Unlock()
}

func (c *Config) Update(dataDir, camera string, hwEncode bool, hwBitrate BitratePreset) error {
	c.mu.Lock()
	c.DataDir = dataDir
	c.Camera = camera
	c.HardwareEncode = hwEncode
	c.HardwareBitrate = ValidBitrate(string(hwBitrate))
	c.mu.Unlock()
	return c.Save()
}
