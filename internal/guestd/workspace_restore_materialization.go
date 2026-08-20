package guestd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	workspaceRestoreJournalVersion = "v0"
	workspaceRestoreStateDir       = "restore-materialization"
	workspaceRestoreJournalName    = "journal.json"
	workspaceRestoreArtifactName   = "target.tar"
	workspaceRestoreTargetDir      = "target"
)

type workspaceRestoreJournal struct {
	Version           string                `json:"version"`
	Phase             string                `json:"phase"`
	CheckpointID      string                `json:"checkpoint_id"`
	SourceWorkspaceID string                `json:"source_workspace_version_id"`
	Target            workspace.ResetTarget `json:"target"`
}

func (entry *workspaceMountEntry) materializeRestoredWorkspace(
	reader io.Reader,
	workspaceID string,
	checkpointID string,
	sourceVersionID string,
	target workspace.ResetTarget,
) error {
	if entry.finalizationRoot == "" {
		return errors.New("restored workspace materialization state root is required")
	}
	stateRoot := filepath.Join(entry.finalizationRoot, workspaceRestoreStateDir)
	journal, found, err := readWorkspaceRestoreJournal(stateRoot)
	if err != nil {
		return err
	}
	want := workspaceRestoreJournal{
		Version: workspaceRestoreJournalVersion, Phase: "prepared",
		CheckpointID: checkpointID, SourceWorkspaceID: sourceVersionID, Target: target,
	}
	if found && !sameWorkspaceRestoreOperation(journal, want) {
		if journal.Phase != "applied" || journal.Target.BaseVersionID != sourceVersionID ||
			entry.baseVersionID != sourceVersionID {
			return errors.New("restored workspace materialization journal conflicts with its exact authority")
		}
		if err := os.RemoveAll(stateRoot); err != nil {
			return fmt.Errorf("prune prior restored workspace materialization: %w", err)
		}
		found = false
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return fmt.Errorf("create restored workspace materialization state: %w", err)
	}
	artifactPath := filepath.Join(stateRoot, workspaceRestoreArtifactName)
	if target.Kind == workspace.ResetTargetArtifact {
		if err := receiveRestoredWorkspaceArtifact(reader, workspaceID, *target.Artifact, artifactPath); err != nil {
			return err
		}
	}
	if !found {
		stagingRoot := filepath.Join(stateRoot, workspaceRestoreTargetDir)
		if err := os.RemoveAll(stagingRoot); err != nil {
			return fmt.Errorf("prune restored workspace target staging: %w", err)
		}
		if err := os.Mkdir(stagingRoot, 0o700); err != nil {
			return fmt.Errorf("create restored workspace target staging: %w", err)
		}
		if target.Kind == workspace.ResetTargetArtifact {
			if err := extractRestoredWorkspaceArtifact(artifactPath, stagingRoot); err != nil {
				return fmt.Errorf("extract restored workspace target staging: %w", err)
			}
		}
		if err := verifyRestoredWorkspaceTree(stagingRoot, target.Tree); err != nil {
			return fmt.Errorf("verify restored workspace target staging: %w", err)
		}
		if err := syncWorkspaceTree(stagingRoot); err != nil {
			return fmt.Errorf("sync restored workspace target staging: %w", err)
		}
		journal = want
		if err := writeWorkspaceRestoreJournal(stateRoot, journal); err != nil {
			return err
		}
	}
	if journal.Phase == "applied" {
		return verifyRestoredWorkspaceTree(entry.workspaceRoot, target.Tree)
	}
	if journal.Phase != "prepared" {
		return errors.New("restored workspace materialization journal phase is invalid")
	}
	if err := pruneRestoredWorkspaceTree(entry.workspaceRoot); err != nil {
		return fmt.Errorf("prune restored workspace live tree: %w", err)
	}
	if target.Kind == workspace.ResetTargetArtifact {
		if err := extractRestoredWorkspaceArtifact(artifactPath, entry.workspaceRoot); err != nil {
			return fmt.Errorf("apply restored workspace target: %w", err)
		}
	}
	if err := verifyRestoredWorkspaceTree(entry.workspaceRoot, target.Tree); err != nil {
		return fmt.Errorf("verify restored workspace live tree: %w", err)
	}
	if err := syncWorkspaceTree(entry.workspaceRoot); err != nil {
		return fmt.Errorf("sync restored workspace live tree: %w", err)
	}
	journal.Phase = "applied"
	return writeWorkspaceRestoreJournal(stateRoot, journal)
}

func sameWorkspaceRestoreOperation(left, right workspaceRestoreJournal) bool {
	return left.Version == right.Version && left.CheckpointID == right.CheckpointID &&
		left.SourceWorkspaceID == right.SourceWorkspaceID && workspace.ResetTargetsEqual(left.Target, right.Target)
}

func receiveRestoredWorkspaceArtifact(reader io.Reader, workspaceID string, artifact workspace.ArtifactIdentity, targetPath string) error {
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		return fmt.Errorf("read restored workspace artifact header: %w", err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact || header.WorkspaceID != workspaceID {
		drainStreamBody(reader, bodyLen)
		return errors.New("restored workspace artifact frame does not match the Workspace target")
	}
	if bodyLen != uint64(artifact.SizeBytes) {
		drainStreamBody(reader, bodyLen)
		return errors.New("restored workspace artifact size does not match the Workspace target")
	}
	if header.BodyDigest != nil && strings.TrimSpace(*header.BodyDigest) != artifact.Digest {
		drainStreamBody(reader, bodyLen)
		return errors.New("restored workspace artifact frame digest does not match the Workspace target")
	}
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".target-*.tar")
	if err != nil {
		drainStreamBody(reader, bodyLen)
		return fmt.Errorf("create restored workspace artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	limited := &io.LimitedReader{R: reader, N: int64(bodyLen)}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), limited)
	if limited.N != 0 && copyErr == nil {
		copyErr = io.ErrUnexpectedEOF
	}
	if syncErr := temporary.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := temporary.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("receive restored workspace artifact: %w", copyErr)
	}
	if written != artifact.SizeBytes || sha256sum.DigestHash(hash) != artifact.Digest {
		return errors.New("restored workspace artifact body does not match the Workspace target")
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return fmt.Errorf("install restored workspace artifact: %w", err)
	}
	return syncDirectory(filepath.Dir(targetPath))
}

func extractRestoredWorkspaceArtifact(path, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = archive.ExtractTarWithStats(file, destination, archive.ExtractOptions{
		MaxBytes: workspace.MaxArtifactExtractedBytes, MaxEntries: workspace.MaxArtifactEntries,
	})
	return err
}

func verifyRestoredWorkspaceTree(root string, want workspace.TreeIdentity) error {
	got, err := workspace.InspectTree(root)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("workspace tree identity = %+v, want %+v", got, want)
	}
	return nil
}

func pruneRestoredWorkspaceTree(liveRoot string) error {
	entries, err := os.ReadDir(liveRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(liveRoot, entry.Name())); err != nil {
			return err
		}
	}
	return syncDirectory(liveRoot)
}

func readWorkspaceRestoreJournal(root string) (workspaceRestoreJournal, bool, error) {
	body, err := os.ReadFile(filepath.Join(root, workspaceRestoreJournalName))
	if errors.Is(err, os.ErrNotExist) {
		return workspaceRestoreJournal{}, false, nil
	}
	if err != nil {
		return workspaceRestoreJournal{}, false, err
	}
	var journal workspaceRestoreJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return workspaceRestoreJournal{}, false, fmt.Errorf("decode restored workspace materialization journal: %w", err)
	}
	return journal, true, nil
}

func writeWorkspaceRestoreJournal(root string, journal workspaceRestoreJournal) error {
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".journal-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(root, workspaceRestoreJournalName)); err != nil {
		return err
	}
	return syncDirectory(root)
}
