package guestd

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func TestWorkspaceCaptureReplaysRetainedArtifactBytes(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	firstResponse, firstBody := runWorkspaceCapture(t, registry, request)
	if firstResponse.GetTree().GetDigest() == "" || firstResponse.GetArtifact().GetDigest() == "" {
		t.Fatalf("Capture response = %+v", firstResponse)
	}
	if err := os.WriteFile(filepath.Join(entry.workspaceRoot, "changed"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondResponse, secondBody := runWorkspaceCapture(t, registry, request)
	if !proto.Equal(firstResponse, secondResponse) {
		t.Fatalf("replayed response changed: first=%+v second=%+v", firstResponse, secondResponse)
	}
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatal("replayed Capture did not return the retained Artifact bytes")
	}

	conflicting := testWorkspaceCaptureRequest(t, authority, "22222222-2222-4222-8222-222222222222")
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, conflicting); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceCaptureConnection(context.Background(), &stream, registry); err != nil {
		t.Fatal(err)
	}
	var conflict workspacev0.CaptureWorkspaceResponse
	if err := frameio.ReadProtoFrame(&stream, &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.GetError() == "" {
		t.Fatal("different operation reused a retained Capture receipt")
	}
}

func TestWorkspaceFinalizationWaitsForProgramRelease(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	releaseProgram, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, release, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind)
		if err == nil {
			release()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("finalization did not wait for Program release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseProgram()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("finalization did not continue after Program release")
	}
}

func TestWorkspaceFinalizationExcludesNewProgramClaim(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	releaseCurrent, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	finalization := make(chan func(), 1)
	go func() {
		_, release, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind)
		if err != nil {
			finalization <- nil
			return
		}
		finalization <- release
	}()
	deadline := time.Now().Add(time.Second)
	for entry.finalizationMu.TryLock() {
		entry.finalizationMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("finalization did not acquire its barrier")
		}
		time.Sleep(time.Millisecond)
	}
	nextAuthority := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	nextAuthority.GetFence().WriterGeneration++
	nextAuthority.GetFence().MountFencingGeneration++
	nextAuthority.GetFence().BaseWorkspaceVersionId = "version-2"
	nextClaim := make(chan error, 1)
	go func() {
		release, err := registry.admitProgram(entry, nextAuthority, time.Now())
		if err == nil {
			release()
		}
		nextClaim <- err
	}()
	releaseCurrent()
	releaseFinalization := <-finalization
	if releaseFinalization == nil {
		t.Fatal("finalization failed")
	}
	select {
	case <-nextClaim:
		t.Fatal("new Program claim crossed the finalization barrier")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFinalization()
	select {
	case err := <-nextClaim:
		if err != nil {
			t.Fatalf("new Program claim was rejected after finalization: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("new Program claim did not continue after finalization")
	}
}

func TestWorkspaceRunAuthorityAdvancesCapturedFrontier(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	runWorkspaceCapture(t, registry, request)

	next := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	next.GetFence().BaseWorkspaceVersionId = "version-2"
	next.GetFence().RunId = "run-2"
	next.GetFence().RunLeaseId = "run-lease-2"
	next.GetFence().WorkspaceLeaseId = "workspace-lease-2"
	if _, err := registry.admitProgram(entry, next, time.Now()); err == nil {
		t.Fatal("non-advancing writer generation replaced captured authority")
	}
	next.GetFence().WriterGeneration++
	next.GetFence().MountFencingGeneration++
	releaseProgram, err := registry.admitProgram(entry, next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	releaseProgram()
	if entry.baseVersionID != "version-2" || entry.finalizing {
		t.Fatalf("advanced frontier = base %q finalizing %t", entry.baseVersionID, entry.finalizing)
	}
	if !proto.Equal(entry.authority, next) {
		t.Fatal("advanced Workspace authority was not installed")
	}
	for _, name := range []string{workspaceFinalizationJournalName, workspaceCaptureArtifactName} {
		if _, err := os.Stat(filepath.Join(entry.finalizationRoot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retained finalization state %q was not pruned: %v", name, err)
		}
	}
}

func TestWorkspaceFinalizationFreezesAuthorityRenewal(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	_, releaseFinalization, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind)
	if err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 1)
	go func() {
		_, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{
			Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
			NewExpiresAtUnixNano: authority.GetFence().GetExpiresAtUnixNano() + int64(time.Minute),
		}, time.Now())
		renewed <- err
	}()
	select {
	case <-renewed:
		t.Fatal("authority renewal crossed the finalization barrier")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFinalization()
	if err := <-renewed; err == nil {
		t.Fatal("authority renewal advanced after finalization")
	}
	firstResponse, firstBody := runWorkspaceCapture(t, registry, request)
	replayedResponse, replayedBody := runWorkspaceCapture(t, registry, request)
	if !proto.Equal(firstResponse, replayedResponse) || !bytes.Equal(firstBody, replayedBody) {
		t.Fatal("authority renewal attempt invalidated retained Capture replay")
	}
}

func TestWorkspaceFinalizationBlocksMountReplacement(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	_, releaseFinalization, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind)
	if err != nil {
		t.Fatal(err)
	}
	replacement := testWorkspaceAuthorityEntry()
	replaced := make(chan struct{})
	go func() {
		registry.register("mount-1", replacement)
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Fatal("Mount replacement crossed the finalization barrier")
	case <-time.After(20 * time.Millisecond):
	}
	registry.mu.Lock()
	current := registry.entries["mount-1"]
	registry.mu.Unlock()
	if current != entry {
		t.Fatal("Mount changed while finalization was active")
	}
	releaseFinalization()
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("Mount replacement did not continue after finalization")
	}
}

func TestWorkspaceProgramAdmissionRejectsRetiredMount(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	replacement := testWorkspaceAuthorityEntry()
	registry.register("mount-1", replacement)
	if _, err := registry.admitProgram(entry, authority, time.Now()); err == nil {
		t.Fatal("Program was admitted on a retired Mount")
	}
}

func TestWorkspaceFinalizationBlocksMountGenerationAdvance(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	_, releaseFinalization, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind)
	if err != nil {
		t.Fatal(err)
	}
	advanced := make(chan bool, 1)
	nextGeneration := entry.currentFencingGeneration() + 1
	go func() {
		_, release, ok := registry.acquire("mount-1", "workspace-1", "channel-1", nextGeneration)
		if ok {
			release()
		}
		advanced <- ok
	}()
	select {
	case <-advanced:
		t.Fatal("mount fencing generation advance crossed the finalization barrier")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFinalization()
	select {
	case ok := <-advanced:
		if ok {
			t.Fatal("mount fencing generation advanced after terminal finalization")
		}
	case <-time.After(time.Second):
		t.Fatal("mount fencing generation advance did not resolve after finalization")
	}
	if entry.currentFencingGeneration() != uint64(authority.GetFence().GetMountFencingGeneration()) {
		t.Fatal("rejected mount fencing generation changed the mounted authority")
	}
}

func TestWorkspaceFinalizationRejectsStaleMountGeneration(t *testing.T) {
	_, registry, authority := testWorkspaceFinalizationMount(t)
	nextGeneration := uint64(authority.GetFence().GetMountFencingGeneration()) + 1
	_, release, ok := registry.acquire("mount-1", "workspace-1", "channel-1", nextGeneration)
	if !ok {
		t.Fatal("failed to advance the mounted fencing generation")
	}
	release()
	request := testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111")
	if _, releaseFinalization, err := beginWorkspaceFinalization(context.Background(), registry, request.GetEnvelope(), workspace.FinalizationCaptureKind); err == nil {
		releaseFinalization()
		t.Fatal("stale Workspace authority began finalization")
	}
}

func TestWorkspaceProgramAdmissionInstallsNewMountGeneration(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	authority.GetFence().MountFencingGeneration++
	releaseProgram, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	releaseProgram()
	if entry.currentFencingGeneration() != uint64(authority.GetFence().GetMountFencingGeneration()) {
		t.Fatalf("installed mount fencing generation = %d", entry.currentFencingGeneration())
	}
}

func runWorkspaceCapture(t *testing.T, registry *workspaceOperationRegistry, request *workspacev0.CaptureWorkspaceRequest) (*workspacev0.CaptureWorkspaceResponse, []byte) {
	t.Helper()
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceCaptureConnection(context.Background(), &stream, registry); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.CaptureWorkspaceResponse
	if err := frameio.ReadProtoFrame(&stream, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetError() != "" {
		t.Fatal(response.GetError())
	}
	header, bodyLength, err := wire.ReadStreamFrameHeader(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact || bodyLength != response.GetArtifact().GetSizeBytes() {
		t.Fatalf("Capture Artifact header = %+v size=%d", header, bodyLength)
	}
	body, err := io.ReadAll(io.LimitReader(&stream, int64(bodyLength)))
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(body)) != bodyLength {
		t.Fatal("Capture Artifact body was truncated")
	}
	return &response, body
}

func testWorkspaceFinalizationMount(t *testing.T) (*workspaceMountEntry, *workspaceOperationRegistry, *workspacev0.WorkspaceRunAuthority) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	releaseProgram, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	releaseProgram()
	return entry, registry, authority
}

func testWorkspaceFinalizationMountUnadmitted(t *testing.T) (*workspaceMountEntry, *workspaceOperationRegistry, *workspacev0.WorkspaceRunAuthority) {
	t.Helper()
	parent := t.TempDir()
	workspaceRoot := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := testWorkspaceAuthorityEntry()
	entry.workspaceRoot = workspaceRoot
	entry.finalizationRoot = stateRoot
	entry.processes = map[string]*workspaceProcess{}
	authority := testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	return entry, registry, authority
}

func testWorkspaceCaptureRequest(t *testing.T, authority *workspacev0.WorkspaceRunAuthority, operationID string) *workspacev0.CaptureWorkspaceRequest {
	t.Helper()
	fence := workspaceFinalizationFence(authority.GetFence())
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: operationID,
		Fence:       fence,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &workspacev0.CaptureWorkspaceRequest{Envelope: &workspacev0.WorkspaceFinalizationEnvelope{
		OperationId:        operationID,
		RequestFingerprint: fingerprint,
		Authority:          proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
	}}
}
