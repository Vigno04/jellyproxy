package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CategoryImages = "images"
	CategoryStatic = "static"
	CategoryMedia  = "media"
	CategoryAll    = "all"

	BytesPerGB = 1024 * 1024 * 1024
)

var ValidCategories = []string{CategoryImages, CategoryStatic, CategoryMedia}

// CacheMetadata stores HTTP response headers and file provenance on disk alongside the .data file
type CacheMetadata struct {
	Key          string              `json:"key"`
	URL          string              `json:"url"`
	Category     string              `json:"category"`
	ItemID       string              `json:"item_id,omitempty"`
	ContentType  string              `json:"content_type"`
	ETag         string              `json:"etag,omitempty"`
	LastModified string              `json:"last_modified,omitempty"`
	Headers      map[string][]string `json:"headers,omitempty"`
	Size         int64               `json:"size"`
	CreatedAt    time.Time           `json:"created_at"`
	LastAccessed time.Time           `json:"last_accessed"`
}

// Entry tracks an in-memory reference to a cached object on disk
type Entry struct {
	Key          string
	Category     string
	ItemID       string
	DataPath     string
	MetaPath     string
	Size         int64
	CreatedAt    time.Time
	LastAccessed time.Time
}

// CategoryStats holds storage usage metrics for a single category
type CategoryStats struct {
	UsedBytes int64   `json:"used_bytes"`
	UsedMB    float64 `json:"used_mb"`
	UsedGB    float64 `json:"used_gb"`
	FileCount int     `json:"file_count"`
}

// StorageStats represents the complete storage telemetry snapshot
type StorageStats struct {
	MaxBytes     int64                    `json:"max_bytes"`
	MaxStorageGB float64                  `json:"max_storage_gb"`
	TotalBytes   int64                    `json:"total_bytes"`
	TotalMB      float64                  `json:"total_mb"`
	TotalGB      float64                  `json:"total_gb"`
	UsagePercent float64                  `json:"usage_percent"`
	FileCount    int                      `json:"file_count"`
	Categories   map[string]CategoryStats `json:"categories"`
}

// StorageManager manages disk cache entries, category accounting, and LRU eviction
type StorageManager struct {
	baseDir         string
	maxStorageBytes int64
	mu              sync.RWMutex
	entries         map[string]*Entry            // key -> Entry
	itemIndex       map[string]map[string]struct{} // itemID -> set of keys
	catStats        map[string]*CategoryStats
	totalBytes      int64
}

// NewStorageManager initializes cache directories, scans existing cached items, and enforces limits
func NewStorageManager(baseDir string, maxStorageGB float64) (*StorageManager, error) {
	if maxStorageGB <= 0 {
		maxStorageGB = 50.0
	}
	maxBytes := int64(maxStorageGB * float64(BytesPerGB))

	sm := &StorageManager{
		baseDir:         baseDir,
		maxStorageBytes: maxBytes,
		entries:         make(map[string]*Entry),
		itemIndex:       make(map[string]map[string]struct{}),
		catStats: map[string]*CategoryStats{
			CategoryImages: {},
			CategoryStatic: {},
			CategoryMedia:  {},
		},
	}

	for _, cat := range ValidCategories {
		dir := filepath.Join(baseDir, cat)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create cache dir %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "tmp"), 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp cache dir: %w", err)
	}

	sm.scanExistingFiles()
	return sm, nil
}

// HashKey generates a deterministic SHA-256 hex digest for a cache key
func HashKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(h[:])
}

func sanitizePrefix(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (sm *StorageManager) buildPaths(category, itemID, keyHash string) (string, string) {
	baseName := keyHash
	if cleanID := sanitizePrefix(itemID); cleanID != "" {
		baseName = cleanID + "_" + keyHash
	}
	dir := filepath.Join(sm.baseDir, category)
	return filepath.Join(dir, baseName+".data"), filepath.Join(dir, baseName+".meta")
}

// scanExistingFiles walks category directories on startup to populate in-memory LRU state
func (sm *StorageManager) scanExistingFiles() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	for _, cat := range ValidCategories {
		catDir := filepath.Join(sm.baseDir, cat)
		entries, err := os.ReadDir(catDir)
		if err != nil {
			continue
		}

		for _, d := range entries {
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".data") {
				continue
			}

			dataPath := filepath.Join(catDir, d.Name())
			baseName := strings.TrimSuffix(d.Name(), ".data")
			metaPath := filepath.Join(catDir, baseName+".meta")

			info, err := d.Info()
			if err != nil {
				continue
			}

			metaBytes, err := os.ReadFile(metaPath)
			if err != nil {
				// Orphaned .data file without .meta; clean it up
				_ = os.Remove(dataPath)
				continue
			}

			var meta CacheMetadata
			if err := json.Unmarshal(metaBytes, &meta); err != nil {
				_ = os.Remove(dataPath)
				_ = os.Remove(metaPath)
				continue
			}

			keyHash := meta.Key
			if keyHash == "" {
				// Fallback to extracting hash from filename
				if idx := strings.LastIndex(baseName, "_"); idx != -1 {
					keyHash = baseName[idx+1:]
					if meta.ItemID == "" {
						meta.ItemID = baseName[:idx]
					}
				} else {
					keyHash = baseName
				}
			}

			lastAcc := meta.LastAccessed
			if lastAcc.IsZero() {
				lastAcc = info.ModTime()
			}
			created := meta.CreatedAt
			if created.IsZero() {
				created = info.ModTime()
			}

			size := info.Size()
			entry := &Entry{
				Key:          keyHash,
				Category:     cat,
				ItemID:       meta.ItemID,
				DataPath:     dataPath,
				MetaPath:     metaPath,
				Size:         size,
				CreatedAt:    created,
				LastAccessed: lastAcc,
			}

			sm.entries[keyHash] = entry
			sm.catStats[cat].UsedBytes += size
			sm.catStats[cat].FileCount++
			sm.totalBytes += size

			if meta.ItemID != "" {
				normID := normalizeItemID(meta.ItemID)
				if sm.itemIndex[normID] == nil {
					sm.itemIndex[normID] = make(map[string]struct{})
				}
				sm.itemIndex[normID][keyHash] = struct{}{}
			}
		}
	}

	log.Printf("[Cache] Initialized storage manager at %s: %d files (%d bytes / %d max bytes)",
		sm.baseDir, len(sm.entries), sm.totalBytes, sm.maxStorageBytes)
}

func normalizeItemID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// Get looks up a cache entry by its key hash, updates LastAccessed, and returns an open file handle and metadata
func (sm *StorageManager) Get(keyHash string) (*os.File, *CacheMetadata, error) {
	sm.mu.Lock()
	entry, exists := sm.entries[keyHash]
	if !exists {
		sm.mu.Unlock()
		return nil, nil, os.ErrNotExist
	}
	now := time.Now().UTC()
	entry.LastAccessed = now
	dataPath := entry.DataPath
	metaPath := entry.MetaPath
	sm.mu.Unlock()

	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		sm.Delete(keyHash)
		return nil, nil, err
	}

	var meta CacheMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		sm.Delete(keyHash)
		return nil, nil, err
	}

	f, err := os.Open(dataPath)
	if err != nil {
		sm.Delete(keyHash)
		return nil, nil, err
	}

	meta.LastAccessed = now
	_ = os.Chtimes(dataPath, now, now)

	return f, &meta, nil
}

// Has checks if an entry with the given keyHash currently exists in cache
func (sm *StorageManager) Has(keyHash string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, exists := sm.entries[keyHash]
	return exists
}

// CreateTempFile creates a temporary file in the cache temp directory for atomic streaming writes
func (sm *StorageManager) CreateTempFile() (*os.File, error) {
	tmpDir := filepath.Join(sm.baseDir, "tmp")
	return os.CreateTemp(tmpDir, "stream-*.tmp")
}

// CommitTempFile atomically moves a written temporary file into the cache store with its metadata and triggers LRU eviction if needed
func (sm *StorageManager) CommitTempFile(tmpPath string, meta CacheMetadata) error {
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if meta.Category == "" {
		meta.Category = CategoryStatic
	}
	if _, ok := sm.catStats[meta.Category]; !ok {
		return fmt.Errorf("invalid cache category: %s", meta.Category)
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	size := info.Size()
	now := time.Now().UTC()

	meta.Size = size
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.LastAccessed = now

	dataPath, metaPath := sm.buildPaths(meta.Category, meta.ItemID, meta.Key)

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	tmpMetaPath := tmpPath + ".meta"
	if err := os.WriteFile(tmpMetaPath, metaBytes, 0644); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmpMetaPath)
	}()

	if err := os.Rename(tmpPath, dataPath); err != nil {
		// Fallback to copy if cross-device (unlikely since both are under sm.baseDir)
		if err := copyFile(tmpPath, dataPath); err != nil {
			return err
		}
	}

	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		_ = os.Remove(dataPath)
		return err
	}

	sm.mu.Lock()
	// If entry already existed, subtract old size first
	if old, exists := sm.entries[meta.Key]; exists {
		sm.removeEntryLocked(old, old.DataPath != dataPath)
	}

	entry := &Entry{
		Key:          meta.Key,
		Category:     meta.Category,
		ItemID:       meta.ItemID,
		DataPath:     dataPath,
		MetaPath:     metaPath,
		Size:         size,
		CreatedAt:    meta.CreatedAt,
		LastAccessed: meta.LastAccessed,
	}

	sm.entries[meta.Key] = entry
	sm.catStats[meta.Category].UsedBytes += size
	sm.catStats[meta.Category].FileCount++
	sm.totalBytes += size

	if meta.ItemID != "" {
		normID := normalizeItemID(meta.ItemID)
		if sm.itemIndex[normID] == nil {
			sm.itemIndex[normID] = make(map[string]struct{})
		}
		sm.itemIndex[normID][meta.Key] = struct{}{}
	}

	needsEvict := sm.maxStorageBytes > 0 && sm.totalBytes > sm.maxStorageBytes
	sm.mu.Unlock()

	if needsEvict {
		sm.EvictLRU()
	}

	return nil
}

// Put writes data directly from an io.Reader into the cache store
func (sm *StorageManager) Put(meta CacheMetadata, r io.Reader) error {
	tmpFile, err := sm.CreateTempFile()
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	_, copyErr := io.Copy(tmpFile, r)
	closeErr := tmpFile.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	return sm.CommitTempFile(tmpPath, meta)
}

// EvictLRU evicts the oldest accessed/modified entries until total usage is at or below 85% of maxStorageBytes
func (sm *StorageManager) EvictLRU() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.maxStorageBytes <= 0 || sm.totalBytes <= sm.maxStorageBytes {
		return 0
	}

	targetBytes := int64(float64(sm.maxStorageBytes) * 0.85)

	list := make([]*Entry, 0, len(sm.entries))
	for _, e := range sm.entries {
		list = append(list, e)
	}

	// Sort oldest LastAccessed first (tie-breaker: CreatedAt)
	sort.Slice(list, func(i, j int) bool {
		if list[i].LastAccessed.Equal(list[j].LastAccessed) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].LastAccessed.Before(list[j].LastAccessed)
	})

	evicted := 0
	for _, e := range list {
		if sm.totalBytes <= targetBytes {
			break
		}
		sm.removeEntryLocked(e, true)
		evicted++
	}

	if evicted > 0 {
		log.Printf("[Cache] LRU eviction complete: removed %d files, current usage %d / %d bytes",
			evicted, sm.totalBytes, sm.maxStorageBytes)
	}
	return evicted
}

// Delete removes a single cache entry by keyHash
func (sm *StorageManager) Delete(keyHash string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	entry, exists := sm.entries[keyHash]
	if !exists {
		return false
	}
	sm.removeEntryLocked(entry, true)
	return true
}

func (sm *StorageManager) removeEntryLocked(entry *Entry, removeFiles bool) {
	if removeFiles {
		_ = os.Remove(entry.DataPath)
		_ = os.Remove(entry.MetaPath)
	}

	delete(sm.entries, entry.Key)

	if st, ok := sm.catStats[entry.Category]; ok {
		st.UsedBytes -= entry.Size
		if st.UsedBytes < 0 {
			st.UsedBytes = 0
		}
		st.FileCount--
		if st.FileCount < 0 {
			st.FileCount = 0
		}
	}

	sm.totalBytes -= entry.Size
	if sm.totalBytes < 0 {
		sm.totalBytes = 0
	}

	if entry.ItemID != "" {
		normID := normalizeItemID(entry.ItemID)
		if keySet, ok := sm.itemIndex[normID]; ok {
			delete(keySet, entry.Key)
			if len(keySet) == 0 {
				delete(sm.itemIndex, normID)
			}
		}
	}
}

// InvalidateByItemID removes all cached entries associated with the given Jellyfin item ID (optionally filtered by type/category)
func (sm *StorageManager) InvalidateByItemID(itemID string, filterType string) int {
	normID := normalizeItemID(itemID)
	if normID == "" {
		return 0
	}

	var targetCat string
	switch strings.ToLower(strings.TrimSpace(filterType)) {
	case "image", "images":
		targetCat = CategoryImages
	case "media", "video", "audio":
		targetCat = CategoryMedia
	case "static":
		targetCat = CategoryStatic
	default:
		targetCat = ""
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	keySet, exists := sm.itemIndex[normID]
	if !exists || len(keySet) == 0 {
		return 0
	}

	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}

	removed := 0
	for _, k := range keys {
		entry, ok := sm.entries[k]
		if !ok {
			continue
		}
		if targetCat != "" && entry.Category != targetCat {
			continue
		}
		sm.removeEntryLocked(entry, true)
		removed++
	}

	if removed > 0 {
		log.Printf("[Cache] Invalidated %d cached entries for item_id=%s (type=%s)", removed, itemID, filterType)
	}
	return removed
}

// Purge clears all cached files for a category ("all", "images", "static", "media")
func (sm *StorageManager) Purge(category string) (int, error) {
	cat := strings.ToLower(strings.TrimSpace(category))
	if cat == "" {
		cat = CategoryAll
	}

	if cat != CategoryAll && cat != CategoryImages && cat != CategoryStatic && cat != CategoryMedia {
		return 0, fmt.Errorf("invalid category %q: must be 'all', 'images', 'static', or 'media'", category)
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	toRemove := make([]*Entry, 0)
	for _, entry := range sm.entries {
		if cat == CategoryAll || entry.Category == cat {
			toRemove = append(toRemove, entry)
		}
	}

	for _, entry := range toRemove {
		sm.removeEntryLocked(entry, true)
	}

	log.Printf("[Cache] Purged %d files from category %q", len(toRemove), cat)
	return len(toRemove), nil
}

// SetMaxStorageGB updates the maximum storage limit at runtime and triggers LRU eviction if usage exceeds the new limit
func (sm *StorageManager) SetMaxStorageGB(gb float64) error {
	if gb <= 0 {
		return fmt.Errorf("max_storage_gb must be greater than 0")
	}
	newMaxBytes := int64(gb * float64(BytesPerGB))

	sm.mu.Lock()
	sm.maxStorageBytes = newMaxBytes
	needsEvict := sm.totalBytes > sm.maxStorageBytes
	sm.mu.Unlock()

	log.Printf("[Cache] Updated max storage limit to %.2f GB (%d bytes)", gb, newMaxBytes)
	if needsEvict {
		sm.EvictLRU()
	}
	return nil
}

// GetStats returns a thread-safe snapshot of current cache storage statistics
func (sm *StorageManager) GetStats() StorageStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	cats := make(map[string]CategoryStats, len(ValidCategories))
	totalFiles := 0
	for _, cat := range ValidCategories {
		st := sm.catStats[cat]
		cats[cat] = CategoryStats{
			UsedBytes: st.UsedBytes,
			UsedMB:    float64(st.UsedBytes) / (1024 * 1024),
			UsedGB:    float64(st.UsedBytes) / float64(BytesPerGB),
			FileCount: st.FileCount,
		}
		totalFiles += st.FileCount
	}

	var pct float64
	if sm.maxStorageBytes > 0 {
		pct = (float64(sm.totalBytes) / float64(sm.maxStorageBytes)) * 100.0
	}

	return StorageStats{
		MaxBytes:     sm.maxStorageBytes,
		MaxStorageGB: float64(sm.maxStorageBytes) / float64(BytesPerGB),
		TotalBytes:   sm.totalBytes,
		TotalMB:      float64(sm.totalBytes) / (1024 * 1024),
		TotalGB:      float64(sm.totalBytes) / float64(BytesPerGB),
		UsagePercent: pct,
		FileCount:    totalFiles,
		Categories:   cats,
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}