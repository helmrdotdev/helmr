//go:build linux || darwin

package controlplane

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// This boundary test uses actual encrypted object bytes, certification, version
// publication and authenticated source delivery. Local reopen is not host-loss
// recovery: only the original published generation exists in remote storage.
func TestPublishedComputerSourceLocalRestore(t *testing.T) {
	f, broker, fence := initialKeyFixture(t)
	remote, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.server.cas = remote
	client := sourceKeyHTTPClient(t, f, broker, fence)
	runtimeID := pgvalue.UUIDString(f.runtime)
	key, err := client.InitialComputerKey(t.Context(), workerapi.InitialComputerKeyRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	disk, err := os.CreateTemp(t.TempDir(), "seed")
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err := disk.Truncate(f.request.LogicalBytes); err != nil {
		t.Fatal(err)
	}
	const offset = 8192
	initial := []byte("retained computer data")
	if _, err := disk.WriteAt(initial, offset); err != nil {
		t.Fatal(err)
	}
	candidate, err := computer.CaptureInitialGeneration(t.Context(), computer.GenerationCapture{
		Disk: disk, Capacity: f.request.LogicalBytes, StagingParent: t.TempDir(), Scope: key.Scope, KeyID: key.ID, Key: key.Key,
		Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 32 << 20, MaxObjects: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	publisher, err := executor.NewInitialGenerationPublisher(client, initialTestObjectPublisher{remote}, runtimeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	locator, err := candidate.Publish(t.Context(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, f.request.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	version, err := client.PublishInitialComputerGeneration(t.Context(), workerapi.InitialComputerGenerationRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 1, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// A lost response can be retried exactly, but cannot publish different bytes.
	replay, err := client.PublishInitialComputerGeneration(t.Context(), workerapi.InitialComputerGenerationRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 1, Root: root})
	if err != nil || replay != version {
		t.Fatalf("publication replay: %v", err)
	}
	changedRoot := root
	changedRoot.LogicalBytes *= 2
	if _, err := client.PublishInitialComputerGeneration(t.Context(), workerapi.InitialComputerGenerationRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 1, Root: changedRoot}); err == nil {
		t.Fatal("different publication accepted after commit")
	}
	if _, err := client.PublishInitialComputerGeneration(t.Context(), workerapi.InitialComputerGenerationRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 2, Root: root}); err == nil {
		t.Fatal("different preparation published")
	}
	computerID, err := ids.Parse(version.ComputerID)
	if err != nil {
		t.Fatal(err)
	}
	versionID, err := ids.Parse(version.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	n, err := q.PinRuntimeComputerSource(t.Context(), db.PinRuntimeComputerSourceParams{
		RuntimeInstanceID: f.runtime, EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(computerID), VersionID: pgvalue.UUID(versionID),
	})
	if err != nil || n != 1 {
		t.Fatalf("pin source: %d %v", n, err)
	}
	fetch := func() workerapi.ComputerSourceMaterial {
		t.Helper()
		source, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{RuntimeInstanceID: runtimeID, DesiredVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if source.Root != root || source.VersionID != version.VersionID {
			source.Clear()
			t.Fatal("source differs from publication")
		}
		return source
	}
	source := fetch()
	keys := make(map[string][]byte, len(source.Keys))
	for _, k := range source.Keys {
		keys[k.ID] = k.Key
	}
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(t.TempDir(), "working"), Base: source.Root, BaseSource: remote,
		Scope: source.Keys[0].Scope, ActiveKey: source.WriteKeyID, Keys: keys, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}
	local, err := computer.CreateLocalGeneration(t.Context(), cfg)
	source.Clear()
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	read := func(d *computer.LocalGeneration, want []byte) {
		t.Helper()
		data := make([]byte, len(want))
		if _, err := d.ReadAt(t.Context(), data, offset); err != nil || !bytes.Equal(data, want) {
			t.Fatalf("restored bytes differ: %v", err)
		}
	}
	read(local, initial)
	changed := []byte("updated computer data")
	if _, err := local.WriteAt(t.Context(), changed, offset); err != nil {
		t.Fatal(err)
	}
	updated, err := local.Flush(t.Context())
	if err != nil || updated == root {
		t.Fatalf("local commit: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	// Reacquire source keys; the first response was erased immediately after open.
	source = fetch()
	defer source.Clear()
	cfg.Keys = make(map[string][]byte, len(source.Keys))
	for _, k := range source.Keys {
		cfg.Keys[k.ID] = k.Key
	}
	reopened, err := computer.OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read(reopened, changed)
	tree, err := computer.OpenGeneration(t.Context(), remote, cfg.Scope, cfg.Keys, root, root.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	original, err := tree.ReadBlock(t.Context(), offset/4096)
	if err != nil || !bytes.Equal(original[:len(initial)], initial) {
		t.Fatalf("local writes changed published source: %v", err)
	}
	if _, err := computer.OpenGeneration(t.Context(), remote, cfg.Scope, cfg.Keys, updated, updated.LogicalBytes); err == nil {
		t.Fatal("local flush claimed remote persistence")
	}
}
