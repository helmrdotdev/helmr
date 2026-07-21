package guestd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	workspaceFinalizationJournalVersion = "v0"
	workspaceFinalizationJournalName    = "journal.json"
	workspaceCaptureArtifactName        = "capture.tar"
	maxWorkspaceFinalizationJournal     = 64 << 10
)

type workspaceFinalizationJournal struct {
	Version            string                        `json:"version"`
	Kind               string                        `json:"kind"`
	OperationID        string                        `json:"operation_id"`
	RequestFingerprint string                        `json:"request_fingerprint"`
	Fence              workspace.FinalizationFence   `json:"fence"`
	Phase              string                        `json:"phase"`
	Tree               workspace.TreeIdentity        `json:"tree"`
	Artifact           workspaceFinalizationArtifact `json:"artifact"`
	PriorTree          *workspace.TreeIdentity       `json:"prior_tree,omitempty"`
	ResetTarget        *workspace.ResetTarget        `json:"reset_target,omitempty"`
}

type workspaceFinalizationArtifact struct {
	Digest     string `json:"digest"`
	MediaType  string `json:"media_type"`
	Encoding   string `json:"encoding"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int    `json:"entry_count"`
}

func handleWorkspaceCaptureConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.CaptureWorkspaceRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read Workspace Capture request: %w", err)
	}
	entry, release, err := beginWorkspaceFinalization(ctx, registry, request.GetEnvelope(), workspace.FinalizationCaptureKind, nil)
	if err != nil {
		return writeWorkspaceCaptureFailure(conn, err)
	}
	defer release()
	response, artifactPath, err := entry.captureWorkspace(request.GetEnvelope())
	if err != nil {
		return writeWorkspaceCaptureFailure(conn, err)
	}
	if err := frameio.WriteProtoFrame(conn, response); err != nil {
		return fmt.Errorf("write Workspace Capture response: %w", err)
	}
	entryCount := int(response.GetArtifact().GetEntryCount())
	if err := wire.WriteFileFrameWithMetadata(conn, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceArtifact,
		WorkspaceID: response.GetReceipt().GetFence().GetWorkspaceId(),
		OperationID: response.GetReceipt().GetOperationId(),
		EntryCount:  &entryCount,
	}, artifactPath, response.GetArtifact().GetDigest(), int64(response.GetArtifact().GetSizeBytes())); err != nil {
		return fmt.Errorf("write Workspace Capture Artifact: %w", err)
	}
	return nil
}

func writeWorkspaceCaptureFailure(conn io.Writer, err error) error {
	if writeErr := frameio.WriteProtoFrame(conn, &workspacev0.CaptureWorkspaceResponse{Error: err.Error()}); writeErr != nil {
		return errors.Join(err, fmt.Errorf("write Workspace Capture failure: %w", writeErr))
	}
	return nil
}

func beginWorkspaceFinalization(ctx context.Context, registry *workspaceOperationRegistry, envelope *workspacev0.WorkspaceFinalizationEnvelope, kind string, target any) (*workspaceMountEntry, func(), error) {
	if envelope == nil || envelope.GetAuthority() == nil || envelope.GetAuthority().GetFence() == nil {
		return nil, func() {}, errors.New("Workspace finalization envelope is required")
	}
	operationID := strings.TrimSpace(envelope.GetOperationId())
	parsedOperationID, err := uuid.Parse(operationID)
	if err != nil || parsedOperationID.String() != operationID {
		return nil, func() {}, errors.New("Workspace finalization operation_id must be a canonical UUID")
	}
	authority := envelope.GetAuthority()
	fence := authority.GetFence()
	entry, releaseEntry, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		authority.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return nil, func() {}, errors.New("Workspace finalization does not match the mounted runtime")
	}
	entry.finalizationMu.Lock()
	if !registry.currentExactLocked(
		entry,
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		authority.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	) {
		entry.finalizationMu.Unlock()
		releaseEntry()
		return nil, func() {}, errors.New("Workspace finalization authority is not current for the Workspace Mount")
	}
	if err := registry.waitForProgramRelease(ctx, entry); err != nil {
		entry.finalizationMu.Unlock()
		releaseEntry()
		return nil, func() {}, fmt.Errorf("wait for Program release: %w", err)
	}
	release := func() {
		entry.finalizationMu.Unlock()
		releaseEntry()
	}
	entry.authorityMu.Lock()
	authorityMatches := workspaceRunAuthoritiesEqual(entry.authority, authority)
	authorityCurrent := entry.authority != nil && entry.authority.GetFence() != nil && entry.authority.GetFence().GetExpiresAtUnixNano() > time.Now().UnixNano()
	entry.authorityMu.Unlock()
	if !authorityMatches || !authorityCurrent {
		release()
		return nil, func() {}, errors.New("Workspace finalization authority is not current")
	}
	projection := workspace.FinalizationRequest{
		OperationID: operationID,
		Fence:       workspaceFinalizationFence(fence),
		Target:      target,
	}
	expectedFingerprint, err := workspace.FinalizationFingerprint(kind, projection)
	if err != nil || envelope.GetRequestFingerprint() != expectedFingerprint {
		release()
		return nil, func() {}, errors.New("Workspace finalization request fingerprint is invalid")
	}
	entry.processesMu.Lock()
	if entry.processAdmissions != 0 || len(entry.processes) != 0 {
		entry.processesMu.Unlock()
		release()
		return nil, func() {}, errors.New("Workspace finalization requires no active exec or PTY")
	}
	entry.finalizing = true
	entry.processesMu.Unlock()
	return entry, release, nil
}

func (entry *workspaceMountEntry) captureWorkspace(envelope *workspacev0.WorkspaceFinalizationEnvelope) (*workspacev0.CaptureWorkspaceResponse, string, error) {
	journal, found, err := entry.readWorkspaceFinalizationJournal()
	if err != nil {
		return nil, "", err
	}
	if found {
		return entry.replayWorkspaceCapture(envelope, journal)
	}
	if entry.finalizationRoot == "" {
		return nil, "", errors.New("Workspace finalization state is unavailable")
	}
	tree, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		return nil, "", fmt.Errorf("inspect captured Workspace tree: %w", err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(entry.workspaceRoot, entry.finalizationRoot, filepath.Dir(entry.workspaceRoot))
	if err != nil {
		return nil, "", err
	}
	defer cleanup()
	if err := syncFile(artifact.Path); err != nil {
		return nil, "", fmt.Errorf("sync Workspace Capture Artifact: %w", err)
	}
	capturePath := filepath.Join(entry.finalizationRoot, workspaceCaptureArtifactName)
	if err := os.Rename(artifact.Path, capturePath); err != nil {
		return nil, "", fmt.Errorf("place Workspace Capture Artifact: %w", err)
	}
	if err := syncDirectory(entry.finalizationRoot); err != nil {
		return nil, "", fmt.Errorf("sync Workspace Capture Artifact directory: %w", err)
	}
	journal = workspaceFinalizationJournal{
		Version:            workspaceFinalizationJournalVersion,
		Kind:               workspace.FinalizationCaptureKind,
		OperationID:        envelope.GetOperationId(),
		RequestFingerprint: envelope.GetRequestFingerprint(),
		Fence:              workspaceFinalizationFence(envelope.GetAuthority().GetFence()),
		Phase:              "committed",
		Tree:               tree,
		Artifact: workspaceFinalizationArtifact{
			Digest:     artifact.Digest,
			MediaType:  artifact.MediaType,
			Encoding:   artifact.Encoding,
			SizeBytes:  artifact.SizeBytes,
			EntryCount: artifact.EntryCount,
		},
	}
	if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
		return nil, "", err
	}
	return workspaceCaptureResponse(journal), capturePath, nil
}

func (entry *workspaceMountEntry) replayWorkspaceCapture(envelope *workspacev0.WorkspaceFinalizationEnvelope, journal workspaceFinalizationJournal) (*workspacev0.CaptureWorkspaceResponse, string, error) {
	if journal.Version != workspaceFinalizationJournalVersion ||
		journal.Kind != workspace.FinalizationCaptureKind ||
		journal.Phase != "committed" ||
		journal.OperationID != envelope.GetOperationId() ||
		journal.RequestFingerprint != envelope.GetRequestFingerprint() ||
		journal.Fence != workspaceFinalizationFence(envelope.GetAuthority().GetFence()) {
		return nil, "", errors.New("Workspace Capture conflicts with the retained finalization receipt")
	}
	path := filepath.Join(entry.finalizationRoot, workspaceCaptureArtifactName)
	if err := verifyWorkspaceCaptureArtifact(path, journal.Artifact); err != nil {
		return nil, "", err
	}
	return workspaceCaptureResponse(journal), path, nil
}

func workspaceCaptureResponse(journal workspaceFinalizationJournal) *workspacev0.CaptureWorkspaceResponse {
	return &workspacev0.CaptureWorkspaceResponse{
		Receipt: &workspacev0.WorkspaceFinalizationReceipt{
			OperationId:        journal.OperationID,
			RequestFingerprint: journal.RequestFingerprint,
			Fence:              workspaceFinalizationFenceProto(journal.Fence),
		},
		Tree: &workspacev0.WorkspaceTreeIdentity{
			Digest:     journal.Tree.Digest,
			SizeBytes:  journal.Tree.SizeBytes,
			EntryCount: uint32(journal.Tree.EntryCount),
		},
		Artifact: &workspacev0.WorkspaceArtifact{
			Digest:     journal.Artifact.Digest,
			MediaType:  journal.Artifact.MediaType,
			Encoding:   journal.Artifact.Encoding,
			SizeBytes:  uint64(journal.Artifact.SizeBytes),
			EntryCount: uint32(journal.Artifact.EntryCount),
		},
	}
}

func (entry *workspaceMountEntry) readWorkspaceFinalizationJournal() (workspaceFinalizationJournal, bool, error) {
	path := filepath.Join(entry.finalizationRoot, workspaceFinalizationJournalName)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return workspaceFinalizationJournal{}, false, nil
	}
	if err != nil {
		return workspaceFinalizationJournal{}, false, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxWorkspaceFinalizationJournal+1))
	if err != nil {
		return workspaceFinalizationJournal{}, false, err
	}
	if len(body) > maxWorkspaceFinalizationJournal {
		return workspaceFinalizationJournal{}, false, errors.New("Workspace finalization journal exceeds its bound")
	}
	var journal workspaceFinalizationJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return workspaceFinalizationJournal{}, false, fmt.Errorf("decode Workspace finalization journal: %w", err)
	}
	return journal, true, nil
}

func (entry *workspaceMountEntry) writeWorkspaceFinalizationJournal(journal workspaceFinalizationJournal) error {
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if len(body) > maxWorkspaceFinalizationJournal {
		return errors.New("Workspace finalization journal exceeds its bound")
	}
	file, err := os.CreateTemp(entry.finalizationRoot, ".journal-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
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
	if err := os.Rename(tempPath, filepath.Join(entry.finalizationRoot, workspaceFinalizationJournalName)); err != nil {
		return err
	}
	return syncDirectory(entry.finalizationRoot)
}

func verifyWorkspaceCaptureArtifact(path string, artifact workspaceFinalizationArtifact) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open retained Workspace Capture Artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != artifact.SizeBytes {
		return errors.New("retained Workspace Capture Artifact size does not match its receipt")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != artifact.Digest {
		return errors.New("retained Workspace Capture Artifact digest does not match its receipt")
	}
	return nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func workspaceFinalizationFence(fence *workspacev0.WorkspaceAuthorityFence) workspace.FinalizationFence {
	if fence == nil {
		return workspace.FinalizationFence{}
	}
	return workspace.FinalizationFence{
		WorkerInstanceID:       fence.GetWorkerInstanceId(),
		WorkerEpoch:            fence.GetWorkerEpoch(),
		RuntimeInstanceID:      fence.GetRuntimeInstanceId(),
		RuntimeIdentityID:      fence.GetRuntimeIdentityId(),
		WorkspaceID:            fence.GetWorkspaceId(),
		WorkspaceMountID:       fence.GetWorkspaceMountId(),
		RunID:                  fence.GetRunId(),
		AttemptNumber:          fence.GetAttemptNumber(),
		RunLeaseID:             fence.GetRunLeaseId(),
		LeaseSequence:          fence.GetLeaseSequence(),
		WorkspaceLeaseID:       fence.GetWorkspaceLeaseId(),
		OwnershipGeneration:    fence.GetOwnershipGeneration(),
		WriterGeneration:       fence.GetWriterGeneration(),
		MountFencingGeneration: fence.GetMountFencingGeneration(),
		ExpiresAtUnixNano:      fence.GetExpiresAtUnixNano(),
		BaseWorkspaceVersionID: fence.GetBaseWorkspaceVersionId(),
	}
}

func workspaceFinalizationFenceProto(fence workspace.FinalizationFence) *workspacev0.WorkspaceAuthorityFence {
	return &workspacev0.WorkspaceAuthorityFence{
		WorkerInstanceId:       fence.WorkerInstanceID,
		WorkerEpoch:            fence.WorkerEpoch,
		RuntimeInstanceId:      fence.RuntimeInstanceID,
		RuntimeIdentityId:      fence.RuntimeIdentityID,
		WorkspaceId:            fence.WorkspaceID,
		WorkspaceMountId:       fence.WorkspaceMountID,
		RunId:                  fence.RunID,
		AttemptNumber:          fence.AttemptNumber,
		RunLeaseId:             fence.RunLeaseID,
		LeaseSequence:          fence.LeaseSequence,
		WorkspaceLeaseId:       fence.WorkspaceLeaseID,
		OwnershipGeneration:    fence.OwnershipGeneration,
		WriterGeneration:       fence.WriterGeneration,
		MountFencingGeneration: fence.MountFencingGeneration,
		ExpiresAtUnixNano:      fence.ExpiresAtUnixNano,
		BaseWorkspaceVersionId: fence.BaseWorkspaceVersionID,
	}
}
