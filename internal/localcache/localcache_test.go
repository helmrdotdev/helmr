package localcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnforceByteLimitIgnoresDotFilesAsStagingFiles(t *testing.T) {
	dir := t.TempDir()
	dotFile := filepath.Join(dir, ".partial")
	oldFile := filepath.Join(dir, "old")
	newFile := filepath.Join(dir, "new")
	writeCacheFile(t, dotFile, "12345678")
	writeCacheFile(t, oldFile, "12345678")
	writeCacheFile(t, newFile, "12345678")
	setModTime(t, oldFile, time.Now().Add(-2*time.Hour))
	setModTime(t, dotFile, time.Now().Add(-time.Hour))
	setModTime(t, newFile, time.Now())

	stats, err := EnforceByteLimit(dir, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.EntriesScanned != 2 || stats.BytesScanned != 16 {
		t.Fatalf("scanned = entries:%d bytes:%d, want only complete cache entries counted", stats.EntriesScanned, stats.BytesScanned)
	}
	if stats.EntriesRemoved != 0 || stats.BytesRemoved != 0 {
		t.Fatalf("removed = entries:%d bytes:%d, want no complete cache entry removed", stats.EntriesRemoved, stats.BytesRemoved)
	}
	if _, err := os.Stat(dotFile); err != nil {
		t.Fatalf("dot file stat: %v", err)
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("old file stat: %v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new file stat: %v", err)
	}
}

func TestEnforceByteLimitPreservesRequestedFiles(t *testing.T) {
	dir := t.TempDir()
	preserved := filepath.Join(dir, "preserved")
	oldFile := filepath.Join(dir, "old")
	newFile := filepath.Join(dir, "new")
	writeCacheFile(t, preserved, "12345678")
	writeCacheFile(t, oldFile, "12345678")
	writeCacheFile(t, newFile, "12345678")
	setModTime(t, oldFile, time.Now().Add(-2*time.Hour))
	setModTime(t, preserved, time.Now().Add(-time.Hour))
	setModTime(t, newFile, time.Now())

	stats, err := EnforceByteLimit(dir, 8, map[string]bool{preserved: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.EntriesScanned != 3 || stats.BytesScanned != 24 {
		t.Fatalf("scanned = entries:%d bytes:%d, want preserved file counted", stats.EntriesScanned, stats.BytesScanned)
	}
	if stats.EntriesRemoved != 2 || stats.BytesRemoved != 16 {
		t.Fatalf("removed = entries:%d bytes:%d, want only evictable files removed", stats.EntriesRemoved, stats.BytesRemoved)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("preserved file stat: %v", err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("old file exists err = %v, want removed", err)
	}
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatalf("new file exists err = %v, want removed", err)
	}
}

func TestTouchMovesFileBehindOlderEvictionCandidates(t *testing.T) {
	dir := t.TempDir()
	touched := filepath.Join(dir, "touched")
	evicted := filepath.Join(dir, "evicted")
	writeCacheFile(t, touched, "12345678")
	writeCacheFile(t, evicted, "12345678")
	old := time.Now().Add(-2 * time.Hour)
	setModTime(t, touched, old)
	setModTime(t, evicted, old.Add(time.Second))
	if err := Touch(touched); err != nil {
		t.Fatal(err)
	}

	stats, err := EnforceByteLimit(dir, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.EntriesRemoved != 1 || stats.BytesRemoved != 8 {
		t.Fatalf("removed = entries:%d bytes:%d, want one file removed", stats.EntriesRemoved, stats.BytesRemoved)
	}
	if _, err := os.Stat(evicted); !os.IsNotExist(err) {
		t.Fatalf("evicted file exists err = %v, want removed", err)
	}
	if _, err := os.Stat(touched); err != nil {
		t.Fatalf("touched file stat: %v", err)
	}
}

func writeCacheFile(t *testing.T, path string, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setModTime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}
