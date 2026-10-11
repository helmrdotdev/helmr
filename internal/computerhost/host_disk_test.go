package computerhost

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestHostDiskRejectsInvalidShapes(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range [][3]int64{{0, 1, 1}, {1, 0, 1}, {1, 1, 0}, {math.MaxInt64, 1, 1}, {1, 1, math.MaxInt64}} {
		if _, err := HostDiskPerSlot(shape[0], shape[1], shape[2], cipher); err == nil {
			t.Fatalf("invalid shape accepted: %v", shape)
		}
	}
}

// The AWS module tests consume these same format fixtures, so codec or artifact
// limit changes cannot silently leave infrastructure admission behind.
func TestHostDiskFormatBounds(t *testing.T) {
	raw, err := os.ReadFile("testdata/host_disk_bounds.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		MemoryMiB  int64 `json:"memory_mib"`
		ScratchMiB int64 `json:"scratch_mib"`
		StagingMiB int64 `json:"staging_mib"`
		Bytes      int64 `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	cipher, _ := NewCheckpointEncryptor(make([]byte, 32))
	for i, fixture := range fixtures {
		actual, err := HostDiskPerSlot(fixture.MemoryMiB, fixture.ScratchMiB, fixture.StagingMiB<<20, cipher)
		if err != nil {
			t.Fatal(err)
		}
		if actual != fixture.Bytes {
			t.Errorf("fixture %d: bytes=%d, recorded=%d", i, actual, fixture.Bytes)
		}
	}
}
