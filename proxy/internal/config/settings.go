package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/Vigno04/jellyproxy/proxy/internal/abr"
	"github.com/Vigno04/jellyproxy/proxy/internal/cache"
)

// RuntimeSettings holds all user-configurable settings managed by the companion
// plugin and the /proxy/ website, persisted across restarts in CacheDir/settings.json.
type RuntimeSettings struct {
	ABR          abr.Config `json:"abr"`
	Encoders     []string   `json:"encoders,omitempty"`
	MaxStorageGB float64    `json:"max_storage_gb"`
}

// SettingsStore persists runtime settings to disk and synchronizes the shaper and cache.
type SettingsStore struct {
	mu      sync.Mutex
	path    string
	abr     *abr.BandwidthManager
	storage *cache.StorageManager
	cfg     *Config
}

// DefaultABRConfig returns the initial shaper configuration before any plugin/website override.
func DefaultABRConfig() abr.Config {
	return abr.Config{
		Enabled:                true,
		UploadSpeedBps:         16_000_000,
		HeadroomPercent:        10,
		MinBitrateBps:          2_000_000,
		Policy:                 abr.PolicyFairShare,
		AllowDegradingExisting: true,
		FastStartSeconds:       10,
		FastStartSharePercent:  90,
		SeekBorrowSeconds:      4,
		BackgroundFloorPercent: 5,
	}
}

// NewSettingsStore loads persisted settings from CacheDir/settings.json (if present)
// and hooks into the bandwidth manager and storage manager.
func NewSettingsStore(cfg *Config, abrMgr *abr.BandwidthManager, storageMgr *cache.StorageManager) *SettingsStore {
	cleanDir := filepath.Clean(cfg.CacheDir)
	s := &SettingsStore{
		path:    filepath.Join(cleanDir, "settings.json"),
		abr:     abrMgr,
		storage: storageMgr,
		cfg:     cfg,
	}
	s.load()
	if abrMgr != nil {
		abrMgr.SetOnChange(s.Save)
	}
	if storageMgr != nil {
		storageMgr.SetOnChange(s.Save)
	}
	return s
}

func (s *SettingsStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var rs RuntimeSettings
	if err := json.Unmarshal(data, &rs); err != nil {
		log.Printf("[JellyProxy] Warning: failed to parse %s: %v", s.path, err)
		return
	}
	if s.abr != nil {
		s.abr.UpdateConfig(rs.ABR)
		if len(rs.Encoders) > 0 {
			s.abr.Capabilities().SetEncoders(rs.Encoders)
		}
	}
	if rs.MaxStorageGB > 0 {
		s.cfg.MaxStorageGB = rs.MaxStorageGB
		if s.storage != nil {
			_ = s.storage.SetMaxStorageGB(rs.MaxStorageGB)
		}
	}
}

// Save atomically writes the current runtime settings to CacheDir/settings.json.
func (s *SettingsStore) Save() {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := RuntimeSettings{
		MaxStorageGB: s.cfg.MaxStorageGB,
	}
	if s.abr != nil {
		rs.ABR = s.abr.Config()
		rs.Encoders = s.abr.Capabilities().Encoders()
	}
	if s.storage != nil {
		stats := s.storage.GetStats()
		if stats.MaxStorageGB > 0 {
			rs.MaxStorageGB = stats.MaxStorageGB
		}
	}

	data, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("[JellyProxy] Warning: failed to write settings file: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[JellyProxy] Warning: failed to commit settings file: %v", err)
	}
}
