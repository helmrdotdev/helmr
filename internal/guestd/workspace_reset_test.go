package guestd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestWorkspaceResetInstallsEmptyTargetAndReplays(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceResetRequest(t, authority, "11111111-1111-4111-8111-111111111111", workspace.ResetTargetProto(mustEmptyResetTarget(t, "version-1")))

	first := runWorkspaceReset(t, registry, request, testWorkspaceRootExchange)
	if first.GetReceipt() == nil || first.GetTarget().GetEmpty() == nil {
		t.Fatalf("Workspace Reset response = %+v", first)
	}
	tree, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Digest != workspace.CanonicalEmptyTreeDigest || tree.EntryCount != 0 {
		t.Fatalf("reset tree = %+v", tree)
	}
	if _, err := os.Stat(entry.workspaceResetStagingPath(request.GetEnvelope().GetOperationId())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("displaced Workspace tree was not cleaned up: %v", err)
	}

	replayed := runWorkspaceReset(t, registry, request, testWorkspaceRootExchange)
	if first.String() != replayed.String() {
		t.Fatalf("replayed Workspace Reset changed: first=%+v replayed=%+v", first, replayed)
	}
}

func TestWorkspaceResetReplacesIncompletePreJournalStaging(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	operationID := "11111111-1111-4111-8111-111111111111"
	staging := entry.workspaceResetStagingPath(operationID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "partial"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := testWorkspaceResetRequest(t, authority, operationID, workspace.ResetTargetProto(mustEmptyResetTarget(t, "version-1")))
	if response := runWorkspaceReset(t, registry, request, testWorkspaceRootExchange); response.GetReceipt() == nil {
		t.Fatalf("Workspace Reset response = %+v", response)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete staging tree remains: %v", err)
	}
}

func TestWorkspaceResetRecoversExchangeBeforeJournalAdvance(t *testing.T) {
	entry, _, authority := testWorkspaceFinalizationMount(t)
	operationID := "11111111-1111-4111-8111-111111111111"
	target := mustEmptyResetTarget(t, "version-1")
	prior, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	staging := entry.workspaceResetStagingPath(operationID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := workspaceFinalizationJournal{
		Version:            workspaceFinalizationJournalVersion,
		Kind:               workspace.FinalizationResetKind,
		OperationID:        operationID,
		RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Fence:              workspaceFinalizationFence(authority.GetFence()),
		Phase:              "prepared",
		PriorTree:          &prior,
		ResetTarget:        &target,
	}
	if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := testWorkspaceRootExchange(entry.workspaceRoot, staging); err != nil {
		t.Fatal(err)
	}
	exchanges := 0
	response, err := entry.advanceWorkspaceReset(journal, func(_, _ string) error {
		exchanges++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if exchanges != 0 || response.GetTarget().GetEmpty() == nil {
		t.Fatalf("recovery exchanges=%d response=%+v", exchanges, response)
	}
	stored, found, err := entry.readWorkspaceFinalizationJournal()
	if err != nil || !found || stored.Phase != "committed" {
		t.Fatalf("recovered journal = %+v found=%t err=%v", stored, found, err)
	}
}

func TestWorkspaceResetInstallsArtifactTarget(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	targetRoot := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "restored"), []byte("target"), 0o640); err != nil {
		t.Fatal(err)
	}
	tree, err := workspace.InspectTree(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(targetRoot, t.TempDir(), filepath.Dir(targetRoot))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	target, err := workspace.ArtifactResetTarget("version-1", tree, workspace.ArtifactIdentity{
		Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
		SizeBytes: artifact.SizeBytes, EntryCount: artifact.EntryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testWorkspaceResetRequest(t, authority, "11111111-1111-4111-8111-111111111111", workspace.ResetTargetProto(target))
	entryCount := artifact.EntryCount
	var stream bytes.Buffer
	if err := wire.WriteFileFrameWithMetadata(&stream, wire.StreamHeader{
		Type: wire.StreamTypeWorkspaceArtifact, WorkspaceID: "workspace-1", WorkspaceMountID: "mount-1",
		OperationID: request.GetEnvelope().GetOperationId(), EntryCount: &entryCount,
	}, artifact.Path, artifact.Digest, artifact.SizeBytes); err != nil {
		t.Fatal(err)
	}
	finalizingEntry, release, err := beginTestWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationResetKind, target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := finalizingEntry.resetWorkspace(&stream, request.GetEnvelope(), target, testWorkspaceRootExchange); err != nil {
		t.Fatal(err)
	}
	installed, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if installed != tree {
		t.Fatalf("installed tree = %+v, want %+v", installed, tree)
	}
	var replay bytes.Buffer
	if err := wire.WriteFileFrameWithMetadata(&replay, wire.StreamHeader{
		Type: wire.StreamTypeWorkspaceArtifact, WorkspaceID: "workspace-1", WorkspaceMountID: "mount-1",
		OperationID: request.GetEnvelope().GetOperationId(), EntryCount: &entryCount,
	}, artifact.Path, artifact.Digest, artifact.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizingEntry.resetWorkspace(&replay, request.GetEnvelope(), target, testWorkspaceRootExchange); err != nil {
		t.Fatalf("replay Workspace Reset: %v", err)
	}
}

func TestWorkspaceResetMarksAmbiguousPreparedStateForRecovery(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	operationID := "11111111-1111-4111-8111-111111111111"
	target := mustEmptyResetTarget(t, "version-1")
	prior, err := workspace.InspectTree(entry.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	staging := entry.workspaceResetStagingPath(operationID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "unexpected"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := workspaceFinalizationJournal{
		Version:            workspaceFinalizationJournalVersion,
		Kind:               workspace.FinalizationResetKind,
		OperationID:        operationID,
		RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Fence:              workspaceFinalizationFence(authority.GetFence()),
		Phase:              "prepared",
		PriorTree:          &prior,
		ResetTarget:        &target,
	}
	if _, err := entry.advanceWorkspaceReset(journal, testWorkspaceRootExchange); err == nil {
		t.Fatal("ambiguous Workspace Reset state was accepted")
	}
	entry.processesMu.Lock()
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if !recoveryRequired {
		t.Fatal("ambiguous Workspace Reset did not fence the Mount")
	}
	next := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	if _, err := registry.admitProgram(entry, next, time.Now()); err == nil {
		t.Fatal("Program was admitted to a recovery_required Mount")
	}
}

func testWorkspaceResetRequest(t *testing.T, authority *workspacev0.WorkspaceRunAuthority, operationID string, target *workspacev0.WorkspaceResetTarget) *workspacev0.ResetWorkspaceRequest {
	t.Helper()
	canonicalTarget, err := workspace.ResetTargetFromProto(target)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationResetKind, workspace.FinalizationRequest{
		OperationID: operationID,
		Fence:       workspaceFinalizationFence(authority.GetFence()),
		Target:      canonicalTarget,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &workspacev0.ResetWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceFinalizationEnvelope{
			OperationId:        operationID,
			RequestFingerprint: fingerprint,
			Authority:          authority,
		},
		Target: target,
	}
}

func runWorkspaceReset(t *testing.T, registry *workspaceOperationRegistry, request *workspacev0.ResetWorkspaceRequest, exchange workspaceRootExchange) *workspacev0.ResetWorkspaceResponse {
	t.Helper()
	target, err := workspace.ResetTargetFromProto(request.GetTarget())
	if err != nil {
		t.Fatal(err)
	}
	entry, release, err := beginTestWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationResetKind, target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	var decoded workspacev0.ResetWorkspaceRequest
	if err := frameio.ReadProtoFrame(&stream, &decoded); err != nil {
		t.Fatal(err)
	}
	response, err := entry.resetWorkspace(&stream, decoded.GetEnvelope(), target, exchange)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func mustEmptyResetTarget(t *testing.T, baseVersionID string) workspace.ResetTarget {
	t.Helper()
	target, err := workspace.EmptyResetTarget(baseVersionID, workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func testWorkspaceRootExchange(left, right string) error {
	temporary := left + ".test-exchange"
	if err := os.Rename(left, temporary); err != nil {
		return err
	}
	if err := os.Rename(right, left); err != nil {
		_ = os.Rename(temporary, left)
		return err
	}
	return os.Rename(temporary, right)
}
