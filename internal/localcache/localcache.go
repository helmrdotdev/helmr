package localcache

import (
	"container/heap"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type EvictionStats struct {
	EntriesScanned int
	BytesScanned   int64
	EntriesRemoved int
	BytesRemoved   int64
}

type fileEntry struct {
	path    string
	size    int64
	modTime time.Time
}

type RootLock struct {
	root string
}

var rootLocks sync.Map

func EnforceByteLimit(root string, maxBytes int64, preserve map[string]bool) (EvictionStats, error) {
	if maxBytes <= 0 {
		return EvictionStats{}, nil
	}
	root = filepath.Clean(root)
	lock := cacheRootLock(root)
	lock.Lock()
	defer lock.Unlock()
	return enforceByteLimitLocked(root, maxBytes, preserve)
}

func WithRootLock(root string, fn func(RootLock) error) error {
	root = filepath.Clean(root)
	lock := cacheRootLock(root)
	lock.Lock()
	defer lock.Unlock()
	return fn(RootLock{root: root})
}

func (l RootLock) EnforceByteLimit(maxBytes int64, preserve map[string]bool) (EvictionStats, error) {
	return enforceByteLimitLocked(l.root, maxBytes, preserve)
}

func enforceByteLimitLocked(root string, maxBytes int64, preserve map[string]bool) (EvictionStats, error) {
	var stats EvictionStats
	if maxBytes <= 0 {
		return stats, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return stats, err
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		stats.EntriesScanned++
		stats.BytesScanned += info.Size()
		return nil
	}); err != nil {
		return stats, err
	}
	if stats.BytesScanned <= maxBytes {
		return stats, nil
	}
	targetRemove := stats.BytesScanned - maxBytes
	var candidates evictionCandidateHeap
	var candidateBytes int64
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if preserve != nil && preserve[filepath.Clean(path)] {
			return nil
		}
		candidate := fileEntry{path: path, size: info.Size(), modTime: info.ModTime()}
		heap.Push(&candidates, candidate)
		candidateBytes += candidate.size
		for targetRemove > 0 && candidates.Len() > 0 {
			newest := candidates[0]
			if candidateBytes-newest.size < targetRemove {
				break
			}
			heap.Pop(&candidates)
			candidateBytes -= newest.size
		}
		return nil
	}); err != nil {
		return stats, err
	}
	total := stats.BytesScanned
	candidatesToRemove := make([]fileEntry, 0, candidates.Len())
	for candidates.Len() > 0 {
		candidatesToRemove = append(candidatesToRemove, heap.Pop(&candidates).(fileEntry))
	}
	for i := len(candidatesToRemove) - 1; i >= 0; i-- {
		if total <= maxBytes {
			break
		}
		entry := candidatesToRemove[i]
		if err := os.Remove(entry.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return stats, err
		}
		total -= entry.size
		stats.EntriesRemoved++
		stats.BytesRemoved += entry.size
	}
	return stats, nil
}

func cacheRootLock(root string) *sync.Mutex {
	value, _ := rootLocks.LoadOrStore(root, &sync.Mutex{})
	return value.(*sync.Mutex)
}

type evictionCandidateHeap []fileEntry

func (h evictionCandidateHeap) Len() int {
	return len(h)
}

func (h evictionCandidateHeap) Less(i int, j int) bool {
	if h[i].modTime.Equal(h[j].modTime) {
		return h[i].path > h[j].path
	}
	return h[i].modTime.After(h[j].modTime)
}

func (h evictionCandidateHeap) Swap(i int, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *evictionCandidateHeap) Push(value any) {
	*h = append(*h, value.(fileEntry))
}

func (h *evictionCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	*h = old[:n-1]
	return value
}

func Touch(path string) error {
	now := time.Now().UTC()
	return os.Chtimes(path, now, now)
}
