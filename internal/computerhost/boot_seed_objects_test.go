package computerhost

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestBootSeedEncryptedPublicationSurvivesBuilderRemoval(t *testing.T) {
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	p := &PreparedMachines{ComputerObjects: preparationOwnerStore{store}, CheckpointCipher: cipher}
	builder, staging := t.TempDir(), t.TempDir()
	raw := bytes.Repeat([]byte("independent guest memory\n"), 200000)
	// The memory format requires pages; keep more than one authenticated chunk.
	raw = raw[:len(raw)/4096*4096]
	file := func(name, media string, contents []byte) vm.SnapshotFile {
		path := filepath.Join(builder, name)
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
		return vm.SnapshotFile{Path: path, MediaType: media}
	}
	artifact := vm.BootSeedArtifact{Manifest: []byte(`{"contract":"helmr.boot-seed.v1"}`), MemoryBytes: int64(len(raw)),
		VMState:     file("state", cas.BootSeedVMStateMediaType, []byte("frozen device state")),
		ScratchDisk: file("scratch", cas.BootSeedScratchMediaType, []byte("packed private scratch")),
		Memory:      file("memory", cas.BootSeedMemoryMediaType, raw)}
	m, err := p.publishBootSeed(t.Context(), staging, uuid.NewV7(), uuid.NewV7(), artifact, 1<<20, p.ComputerObjects)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(builder); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	body, err := store.Get(t.Context(), m.Memory.Digest)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := io.ReadAll(body)
	if closeErr := body.Close(); err != nil || closeErr != nil {
		t.Fatalf("read published memory: %v %v", err, closeErr)
	}
	if bytes.Contains(sealed, raw[:4096]) {
		t.Fatal("published memory contains plaintext")
	}
	ranges := &memoryRanges{data: sealed}
	p.ComputerRanges = ranges
	restored, err := p.openBootSeed(t.Context(), t.TempDir(), m, m.MemoryBytes, m.ScratchBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ranges.reads != 2 {
		t.Fatalf("opening seed fetched memory payloads: %d reads", ranges.reads)
	}
	got := make([]byte, 4096)
	if _, err := restored.MemorySource.ReadAt(t.Context(), got, memoryChunkSize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw[memoryChunkSize:memoryChunkSize+4096]) {
		t.Fatal("demanded memory differs from captured bytes")
	}
	for _, test := range []struct {
		name string
		edit func(*bootseed.Manifest)
	}{
		{"build", func(m *bootseed.Manifest) { m.BuildID = uuid.NewV7() }},
		{"environment", func(m *bootseed.Manifest) { m.EnvironmentID = uuid.NewV7() }},
		{"content", func(m *bootseed.Manifest) { m.Config = []byte(`{"other":"content"}`) }},
		{"continuation-media", func(m *bootseed.Manifest) { m.Memory.MediaType = cas.CheckpointMemoryMediaType }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := m
			test.edit(&changed)
			if _, err := p.openBootSeed(t.Context(), t.TempDir(), changed, m.MemoryBytes, m.ScratchBytes); err == nil {
				t.Fatal("rebound seed authenticated")
			}
		})
	}
	if _, err := p.openBootSeed(t.Context(), t.TempDir(), m, m.MemoryBytes+4096, m.ScratchBytes); err == nil {
		t.Fatal("seed accepted a different memory shape")
	}
	rotated, err := NewCheckpointEncryptor(bytes.Repeat([]byte{20}, 32))
	if err != nil {
		t.Fatal(err)
	}
	other := &PreparedMachines{ComputerObjects: p.ComputerObjects, ComputerRanges: p.ComputerRanges, CheckpointCipher: rotated}
	if _, err := other.openBootSeed(t.Context(), t.TempDir(), m, m.MemoryBytes, m.ScratchBytes); !errors.Is(err, ErrCheckpointKeyUnavailable) {
		t.Fatalf("parallel seed staging lost key mismatch: %v", err)
	}
	if _, err := openCheckpointMemory(t.Context(), ranges, cipher, m.Memory, m.MemoryHeaderDigest, m.BuildID.String()+"/memory", m.MemoryBytes); err == nil {
		t.Fatal("seed authenticated as a customer continuation")
	}
	p.OptionalComputerObjects = p.ComputerObjects
	prepared, err := p.openPreparedBootSeed(t.Context(), t.TempDir(), m, m.MemoryBytes, m.ScratchBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), m.Memory.Digest); err != nil {
		t.Fatal(err)
	}
	clear(got)
	if _, err := prepared.MemorySource.ReadAt(t.Context(), got, memoryChunkSize); err != nil {
		t.Fatal("prepared pager retained a remote dependency", err)
	}
	if !bytes.Equal(got, raw[memoryChunkSize:memoryChunkSize+4096]) {
		t.Fatal("local prepared memory differs")
	}
}
