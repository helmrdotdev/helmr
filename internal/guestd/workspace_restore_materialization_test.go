package guestd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestMaterializeRestoredWorkspaceReplaysPreparedAndAppliedJournal(t *testing.T) {
	liveRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(liveRoot, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizationRoot := t.TempDir()
	stateRoot := filepath.Join(finalizationRoot, workspaceRestoreStateDir)
	if err := os.MkdirAll(filepath.Join(stateRoot, workspaceRestoreTargetDir), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := workspace.EmptyResetTarget("target-version", workspace.TreeIdentity{
		Digest: workspace.CanonicalEmptyTreeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	journal := workspaceRestoreJournal{
		Version: workspaceRestoreJournalVersion, Phase: "prepared",
		CheckpointID: "checkpoint-b", SourceWorkspaceID: "source-version", Target: target,
	}
	if err := writeWorkspaceRestoreJournal(stateRoot, journal); err != nil {
		t.Fatal(err)
	}
	entry := workspaceMountEntry{
		workspaceRoot: liveRoot, finalizationRoot: finalizationRoot,
		baseVersionID: "source-version",
	}
	if err := entry.materializeRestoredWorkspace(
		bytes.NewReader(nil), "workspace-1", "checkpoint-b", "source-version", target,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared replay left the old tree: %v", err)
	}
	if err := entry.materializeRestoredWorkspace(
		bytes.NewReader(nil), "workspace-1", "checkpoint-b", "source-version", target,
	); err != nil {
		t.Fatalf("applied replay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, "drift.txt"), []byte("drift"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := entry.materializeRestoredWorkspace(
		bytes.NewReader(nil), "workspace-1", "checkpoint-b", "source-version", target,
	); err == nil {
		t.Fatal("applied journal accepted a drifted live tree")
	}
}

func TestPruneRestoredWorkspaceTreeRemovesLiveEntries(t *testing.T) {
	liveRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(liveRoot, "swap"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, "swap", "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(liveRoot, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneRestoredWorkspaceTree(liveRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, "swap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory-to-file kind change was not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, "keep")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matching directory was not pruned: %v", err)
	}
}

func TestReceiveRestoredWorkspaceArtifactRejectsDigestMismatch(t *testing.T) {
	body := []byte("tar")
	wantDigest := sha256sum.DigestBytes(body)
	wrongDigest := sha256sum.DigestBytes([]byte("wrong"))
	var stream bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&stream, wire.StreamHeader{
		Type: wire.StreamTypeWorkspaceArtifact, WorkspaceID: "workspace-1",
		BodyDigest: &wrongDigest,
	}, uint64(len(body))); err != nil {
		t.Fatal(err)
	}
	stream.Write(body)
	err := receiveRestoredWorkspaceArtifact(
		&stream,
		"workspace-1",
		workspace.ArtifactIdentity{Digest: wantDigest, SizeBytes: int64(len(body))},
		filepath.Join(t.TempDir(), "target.tar"),
	)
	if err == nil {
		t.Fatal("mismatched artifact digest was accepted")
	}
}
