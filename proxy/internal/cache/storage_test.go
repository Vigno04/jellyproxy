package cache

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStorageManagerAddGetEvictPurgeInvalidate(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "jellyproxy-cache-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize with 1000 bytes limit (~1KB)
	sm, err := NewStorageManager(tmpDir, 1000.0/float64(BytesPerGB))
	if err != nil {
		t.Fatalf("NewStorageManager failed: %v", err)
	}

	// 1. Add an image entry (400 bytes)
	key1 := HashKey("images:/Items/item-123/Images/Primary")
	data1 := bytes.Repeat([]byte("A"), 400)
	meta1 := CacheMetadata{
		Key:         key1,
		URL:         "/Items/item-123/Images/Primary",
		Category:    CategoryImages,
		ItemID:      "item-123",
		ContentType: "image/jpeg",
		ETag:        `"etag1"`,
		CreatedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}
	if err := sm.Put(meta1, bytes.NewReader(data1)); err != nil {
		t.Fatalf("Put 1 failed: %v", err)
	}

	// Verify Get
	f, gotMeta, err := sm.Get(key1)
	if err != nil {
		t.Fatalf("Get 1 failed: %v", err)
	}
	f.Close()
	if gotMeta.ContentType != "image/jpeg" || gotMeta.ItemID != "item-123" {
		t.Fatalf("unexpected metadata: %+v", gotMeta)
	}

	// 2. Add a static entry (400 bytes) with older access time
	key2 := HashKey("static:/web/main.bundle.js")
	data2 := bytes.Repeat([]byte("B"), 400)
	meta2 := CacheMetadata{
		Key:         key2,
		URL:         "/web/main.bundle.js",
		Category:    CategoryStatic,
		ContentType: "application/javascript",
	}
	if err := sm.Put(meta2, bytes.NewReader(data2)); err != nil {
		t.Fatalf("Put 2 failed: %v", err)
	}
	// Manually backdate entry1 so it is the oldest accessed
	sm.mu.Lock()
	sm.entries[key1].LastAccessed = time.Now().UTC().Add(-1 * time.Hour)
	sm.mu.Unlock()

	// 3. Add another image entry (400 bytes) -> total 1200 > 1000 max -> should trigger LRU eviction down to <= 850 bytes (evicts key1)
	key3 := HashKey("images:/Items/item-456/Images/Backdrop")
	data3 := bytes.Repeat([]byte("C"), 400)
	meta3 := CacheMetadata{
		Key:         key3,
		URL:         "/Items/item-456/Images/Backdrop",
		Category:    CategoryImages,
		ItemID:      "item-456",
		ContentType: "image/png",
	}
	if err := sm.Put(meta3, bytes.NewReader(data3)); err != nil {
		t.Fatalf("Put 3 failed: %v", err)
	}

	stats := sm.GetStats()
	if stats.TotalBytes != 800 || stats.FileCount != 2 {
		t.Fatalf("expected 800 bytes and 2 files after LRU eviction, got %+v", stats)
	}
	if _, _, err := sm.Get(key1); err == nil {
		t.Fatalf("expected key1 to be evicted")
	}

	// 4. Test InvalidateByItemID for item-456
	removed := sm.InvalidateByItemID("item-456", "image")
	if removed != 1 {
		t.Fatalf("expected 1 invalidated file, got %d", removed)
	}
	if _, _, err := sm.Get(key3); err == nil {
		t.Fatalf("expected key3 to be invalidated")
	}

	// 5. Test Purge by category
	purged, err := sm.Purge(CategoryStatic)
	if err != nil || purged != 1 {
		t.Fatalf("expected 1 purged static file, got %d (err=%v)", purged, err)
	}
	stats = sm.GetStats()
	if stats.TotalBytes != 0 || stats.FileCount != 0 {
		t.Fatalf("expected empty cache after purge, got %+v", stats)
	}
}

func TestClassifyAndScanReload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "jellyproxy-scan-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm, err := NewStorageManager(tmpDir, 1.0)
	if err != nil {
		t.Fatalf("NewStorageManager failed: %v", err)
	}

	key := HashKey("images:/Items/abc-def/Images/Primary")
	meta := CacheMetadata{
		Key:         key,
		URL:         "/Items/abc-def/Images/Primary",
		Category:    CategoryImages,
		ItemID:      "abc-def",
		ContentType: "image/webp",
	}
	if err := sm.Put(meta, strings.NewReader("hello world image data")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Re-instantiate StorageManager on same directory to verify startup scan
	sm2, err := NewStorageManager(tmpDir, 1.0)
	if err != nil {
		t.Fatalf("NewStorageManager 2 failed: %v", err)
	}
	st := sm2.GetStats()
	if st.FileCount != 1 || st.Categories[CategoryImages].FileCount != 1 {
		t.Fatalf("expected scanned file count 1, got %+v", st)
	}
	if removed := sm2.InvalidateByItemID("ABC-DEF", "image"); removed != 1 {
		t.Fatalf("expected invalidation after scan reload to remove 1 file, got %d", removed)
	}
}