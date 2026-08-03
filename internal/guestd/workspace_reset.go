package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type workspaceRootExchange func(string, string) error

func handleWorkspaceResetConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	var request workspacev0.ResetWorkspaceRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace reset request: %w", err)
	}
	target, err := workspace.ResetTargetFromProto(request.GetTarget())
	if err != nil {
		return writeWorkspaceResetFailure(conn, err)
	}
	if request.GetEnvelope() == nil || request.GetEnvelope().GetAuthority() == nil || request.GetEnvelope().GetAuthority().GetFence() == nil || target.BaseVersionID != request.GetEnvelope().GetAuthority().GetFence().GetBaseWorkspaceVersionId() {
		return writeWorkspaceResetFailure(conn, errors.New("workspace reset target does not match the admitted base version"))
	}
	entry, release, err := acquireWorkspaceFinalization(ctx, registry, request.GetEnvelope(), workspace.FinalizationResetKind, target)
	if err != nil {
		return writeWorkspaceResetFailure(conn, err)
	}
	defer release()
	response, err := entry.resetWorkspace(conn, request.GetEnvelope(), target, exchangeWorkspaceRoots)
	if err != nil {
		return writeWorkspaceResetFailure(conn, err)
	}
	if err := frameio.WriteProtoFrame(conn, response); err != nil {
		return fmt.Errorf("write workspace reset response: %w", err)
	}
	return nil
}

func writeWorkspaceResetFailure(conn io.Writer, err error) error {
	if writeErr := frameio.WriteProtoFrame(conn, &workspacev0.ResetWorkspaceResponse{Error: err.Error()}); writeErr != nil {
		return errors.Join(err, fmt.Errorf("write workspace reset failure: %w", writeErr))
	}
	return nil
}

func (entry *workspaceMountEntry) resetWorkspace(conn io.Reader, envelope *workspacev0.WorkspaceFinalizationEnvelope, target workspace.ResetTarget, exchange workspaceRootExchange) (*workspacev0.ResetWorkspaceResponse, error) {
	journal, found, err := entry.readWorkspaceFinalizationJournal()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("workspace reset requires a retained begin receipt")
	}
	if journal.Phase != "begun" {
		if err := validateWorkspaceResetJournal(journal, envelope, target); err != nil {
			return nil, err
		}
		if target.Kind == workspace.ResetTargetArtifact {
			if err := receiveWorkspaceResetArtifact(conn, envelope, target, ""); err != nil {
				return nil, err
			}
		}
		return entry.advanceWorkspaceReset(journal, exchange)
	}

	priorTree, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace reset prior tree: %w", err)
	}
	staging := entry.workspaceResetStagingPath(envelope.GetOperationId())
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("remove incomplete workspace reset staging root: %w", err)
	}
	if err := syncDirectory(filepath.Dir(entry.workspaceRoot)); err != nil {
		return nil, fmt.Errorf("sync removed workspace reset staging root: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace reset staging root: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if target.Kind == workspace.ResetTargetArtifact {
		if err := receiveWorkspaceResetArtifact(conn, envelope, target, staging); err != nil {
			return nil, err
		}
	}
	stagedTree, err := workspace.InspectTree(staging)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace reset target tree: %w", err)
	}
	if stagedTree != target.Tree {
		return nil, errors.New("workspace reset staged tree does not match the target identity")
	}
	if err := syncWorkspaceTree(staging); err != nil {
		return nil, fmt.Errorf("sync workspace reset target tree: %w", err)
	}
	journal = workspaceFinalizationJournal{
		Version:            workspaceFinalizationJournalVersion,
		Kind:               workspace.FinalizationResetKind,
		OperationID:        envelope.GetOperationId(),
		RequestFingerprint: envelope.GetRequestFingerprint(),
		Fence:              workspaceFinalizationFence(envelope.GetAuthority().GetFence()),
		Phase:              "prepared",
		PriorTree:          &priorTree,
		ResetTarget:        &target,
	}
	if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
		return nil, err
	}
	removeStaging = false
	return entry.advanceWorkspaceReset(journal, exchange)
}

func validateWorkspaceResetJournal(journal workspaceFinalizationJournal, envelope *workspacev0.WorkspaceFinalizationEnvelope, target workspace.ResetTarget) error {
	if journal.Version != workspaceFinalizationJournalVersion ||
		journal.Kind != workspace.FinalizationResetKind ||
		journal.OperationID != envelope.GetOperationId() ||
		journal.RequestFingerprint != envelope.GetRequestFingerprint() ||
		journal.Fence != workspaceFinalizationFence(envelope.GetAuthority().GetFence()) ||
		journal.PriorTree == nil || journal.ResetTarget == nil || !workspace.ResetTargetsEqual(*journal.ResetTarget, target) {
		return errors.New("workspace reset conflicts with the retained finalization receipt")
	}
	switch journal.Phase {
	case "prepared", "exchanged", "committed":
		return nil
	default:
		return errors.New("workspace reset journal phase is invalid")
	}
}

func (entry *workspaceMountEntry) advanceWorkspaceReset(journal workspaceFinalizationJournal, exchange workspaceRootExchange) (*workspacev0.ResetWorkspaceResponse, error) {
	target := *journal.ResetTarget
	staging := entry.workspaceResetStagingPath(journal.OperationID)
	switch journal.Phase {
	case "prepared":
		liveTree, err := workspace.InspectTree(entry.workspaceRoot)
		if err != nil {
			return nil, entry.requireWorkspaceRecovery(fmt.Errorf("inspect live workspace reset tree: %w", err))
		}
		if liveTree != target.Tree {
			stagedTree, err := workspace.InspectTree(staging)
			if err != nil || liveTree != *journal.PriorTree || stagedTree != target.Tree {
				return nil, entry.requireWorkspaceRecovery(errors.New("workspace reset prepared state does not match its journal"))
			}
			if err := exchange(entry.workspaceRoot, staging); err != nil {
				return nil, fmt.Errorf("exchange workspace reset roots: %w", err)
			}
			if err := syncDirectory(filepath.Dir(entry.workspaceRoot)); err != nil {
				return nil, entry.requireWorkspaceRecovery(fmt.Errorf("sync exchanged workspace reset roots: %w", err))
			}
		}
		journal.Phase = "exchanged"
		if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
			return nil, err
		}
		fallthrough
	case "exchanged":
		liveTree, err := workspace.InspectTree(entry.workspaceRoot)
		if err != nil || liveTree != target.Tree {
			return nil, entry.requireWorkspaceRecovery(errors.New("workspace reset exchanged tree does not match its target"))
		}
		journal.Phase = "committed"
		if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
			return nil, err
		}
		fallthrough
	case "committed":
		liveTree, err := workspace.InspectTree(entry.workspaceRoot)
		if err != nil || liveTree != target.Tree {
			return nil, entry.requireWorkspaceRecovery(errors.New("committed workspace reset tree does not match its target"))
		}
		if err := os.RemoveAll(staging); err == nil {
			_ = syncDirectory(filepath.Dir(entry.workspaceRoot))
		}
		return workspaceResetResponse(journal), nil
	default:
		return nil, errors.New("workspace reset journal phase is invalid")
	}
}

func (entry *workspaceMountEntry) requireWorkspaceRecovery(err error) error {
	entry.processesMu.Lock()
	entry.recoveryRequired = true
	entry.processesMu.Unlock()
	return err
}

func (entry *workspaceMountEntry) workspaceResetStagingPath(operationID string) string {
	return filepath.Join(filepath.Dir(entry.workspaceRoot), ".helmr-workspace-reset-"+operationID)
}

func receiveWorkspaceResetArtifact(conn io.Reader, envelope *workspacev0.WorkspaceFinalizationEnvelope, target workspace.ResetTarget, destination string) error {
	header, bodyLength, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return fmt.Errorf("read workspace reset artifact header: %w", err)
	}
	artifact := target.Artifact
	if artifact == nil || header.Type != wire.StreamTypeWorkspaceArtifact ||
		header.WorkspaceID != envelope.GetAuthority().GetFence().GetWorkspaceId() ||
		header.WorkspaceMountID != envelope.GetAuthority().GetFence().GetWorkspaceMountId() ||
		header.OperationID != envelope.GetOperationId() ||
		header.BodyDigest == nil || strings.TrimSpace(*header.BodyDigest) != artifact.Digest ||
		header.EntryCount == nil || *header.EntryCount != artifact.EntryCount ||
		bodyLength != uint64(artifact.SizeBytes) {
		return errors.New("workspace reset artifact frame does not match its target")
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLength)}
	hashed := newDigestingReader(body)
	if destination == "" {
		if _, err := io.Copy(io.Discard, hashed); err != nil {
			return fmt.Errorf("read replayed workspace reset artifact: %w", err)
		}
	} else {
		stats, err := archive.ExtractTarWithStats(hashed, destination, archive.ExtractOptions{
			MaxBytes:   workspace.MaxArtifactExtractedBytes,
			MaxEntries: workspace.MaxArtifactEntries,
		})
		if err != nil {
			_, drainErr := io.Copy(io.Discard, hashed)
			return errors.Join(fmt.Errorf("extract workspace reset artifact: %w", err), drainErr)
		}
		if stats.EntryCount != artifact.EntryCount {
			return errors.New("workspace reset artifact entry count does not match its target")
		}
	}
	if body.N != 0 {
		return errors.New("workspace reset artifact stream ended early")
	}
	if hashed.Digest() != artifact.Digest {
		return errors.New("workspace reset artifact digest does not match its target")
	}
	return nil
}

func syncWorkspaceTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			return syncFile(path)
		case info.IsDir():
			directories = append(directories, path)
		case info.Mode()&os.ModeSymlink != 0:
			return nil
		default:
			return fmt.Errorf("unsupported workspace reset entry %q", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return syncDirectory(filepath.Dir(root))
}

func workspaceResetResponse(journal workspaceFinalizationJournal) *workspacev0.ResetWorkspaceResponse {
	return &workspacev0.ResetWorkspaceResponse{
		Receipt: &workspacev0.WorkspaceFinalizationReceipt{
			OperationId:        journal.OperationID,
			RequestFingerprint: journal.RequestFingerprint,
			Fence:              workspaceFinalizationFenceProto(journal.Fence),
		},
		Target: workspace.ResetTargetProto(*journal.ResetTarget),
	}
}
