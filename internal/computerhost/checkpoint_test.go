package computerhost

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func TestComputerCheckpointerCreatesManifestAndCleansSnapshotFiles(t *testing.T) {
	target := checkpointCaptureTarget(2)
	stream := checkpointFreezeStream(t, target)
	artifact := checkpointArtifact(t)
	machine := &checkpointMachine{stream: stream, artifact: artifact}
	store := &checkpointCAS{}
	encryptor := testCheckpointEncryptor(t)

	result, err := (&computerCheckpointer{publication: testCheckpointPublication,
		machine: machine,
		objects: store, reservations: testCheckpointReservations(t),
		encryptor: encryptor,
		tempDir:   t.TempDir(),
		computer:  testCheckpointComputerBase(),
	}).CreateCheckpoint(context.Background(), computerCheckpointRequest{
		Register: func(context.Context, workerapi.CheckpointManifest) error { return nil }, Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.Manifest

	if machine.resumeCount != 0 || machine.closeCount != 0 || len(machine.snapshotRequests) != 1 || machine.snapshotRequests[0].ID != "checkpoint" {
		t.Fatalf("session = %+v", machine)
	}
	if err := (&computerCheckpointer{publication: testCheckpointPublication, machine: machine}).ReleaseCheckpointSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if machine.closeCount != 1 {
		t.Fatalf("session close count = %d, want 1", machine.closeCount)
	}
	if stream.closed != 1 {
		t.Fatalf("stream closed %d times", stream.closed)
	}
	assertComputerFreezeFrame(t, stream.written.Bytes(), target)
	if len(store.puts) != 4 {
		t.Fatalf("puts = %+v", store.puts)
	}
	manifestPut := checkpointPutByMediaType(t, store, cas.CheckpointVMConfigMediaType)
	vmStatePut := checkpointPutByMediaType(t, store, cas.CheckpointVMStateMediaType)
	scratchPut := checkpointPutByMediaType(t, store, cas.CheckpointScratchDiskMediaType)
	memoryPut := checkpointPutByMediaType(t, store, cas.CheckpointMemoryMediaType)
	if manifest.RecoveryPoint.Runtime.Backend != "firecracker" || manifest.RecoveryPoint.Runtime.Arch != "x86_64" || manifest.RecoveryPoint.Runtime.Contract != "helmr.vm-runtime.v0" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if manifest.RecoveryPoint.ID != "checkpoint" || len(manifest.RecoveryPoint.Runs) != 2 || manifest.RecoveryPoint.Runs[0].RunWaitID != "wait-a" {
		t.Fatalf("recovery point = %+v", manifest.RecoveryPoint)
	}
	if manifest.RecoveryPoint.Runtime.KernelDigest != "sha256:kernel" || manifest.RecoveryPoint.Runtime.RootfsDigest != "sha256:rootfs" {
		t.Fatalf("manifest digests = %+v", manifest)
	}
	if manifest.RecoveryPoint.Runtime.ConfigDigest != "sha256:runtime-config" {
		t.Fatalf("runtime config digest = %+v", manifest.RecoveryPoint.Runtime.ConfigDigest)
	}
	if manifest.RecoveryPoint.Runtime.VMVCPUCount != 2 ||
		manifest.RecoveryPoint.Runtime.CPUConfigDigest != sha256sum.DigestBytes([]byte("cpu-config")) {
		t.Fatalf("runtime CPU shape = %+v", manifest.RecoveryPoint.Runtime)
	}
	if manifest.RuntimeState.ConfigArtifact.Digest != manifestPut.object.Digest {
		t.Fatalf("manifest artifact = %+v puts=%+v", manifest.RuntimeState.ConfigArtifact, store.puts)
	}
	if manifest.RuntimeState.VMStateArtifact.Digest != vmStatePut.object.Digest {
		t.Fatalf("vm state artifact = %+v puts=%+v", manifest.RuntimeState.VMStateArtifact, store.puts)
	}
	if manifest.RuntimeState.ScratchDiskArtifact.Digest != scratchPut.object.Digest {
		t.Fatalf("scratch disk artifact = %+v puts=%+v", manifest.RuntimeState.ScratchDiskArtifact, store.puts)
	}
	if len(manifest.RuntimeState.MemoryArtifacts) != 1 || manifest.RuntimeState.MemoryArtifacts[0].Digest != memoryPut.object.Digest {
		t.Fatalf("memory artifacts = %+v puts=%+v", manifest.RuntimeState.MemoryArtifacts, store.puts)
	}
	if manifest.ComputerState.Base.MountPath != "/workspace" {
		t.Fatalf("computer base = %+v", manifest.ComputerState.Base)
	}
	if string(manifest.RuntimeState.Config) != `{"runtime":{"backend":"firecracker"}}` {
		t.Fatalf("raw manifest = %s", manifest.RuntimeState.Config)
	}
	if !checkpointPhaseHasFilepackStats(manifest.Phases, "pack_scratch_filepack") {
		t.Fatalf("manifest phases missing scratch filepack stats: %+v", manifest.Phases)
	}
	assertRemoved(t, artifact.VMState.Path)
	assertRemoved(t, artifact.ScratchDisk.Path)
	assertRemoved(t, artifact.Memory[0].Path)
}

type checkpointStream struct {
	*scriptedGuestStream
	closeErr error
	closed   int
}

func newCheckpointStream(t *testing.T, closeErr error, messages ...proto.Message) *checkpointStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &checkpointStream{scriptedGuestStream: &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}, closeErr: closeErr}
}

func testCheckpointComputerBase() workerapi.CheckpointComputerBase {
	return workerapi.CheckpointComputerBase{

		MountPath: "/workspace",
	}
}

func (s *checkpointStream) Close() error {
	if s.closed > 0 {
		return nil
	}
	s.closed += 1
	if s.closeErr != nil {
		return s.closeErr
	}
	return nil
}

type checkpointMachine struct {
	stream           io.ReadWriteCloser
	artifact         vm.SnapshotArtifact
	snapshotErr      error
	snapshotRequests []vm.SnapshotRequest
	resumeCount      int
	closeCount       int
	closeErr         error
	closed           bool
	snapshotHook     func()
}

func (s *checkpointMachine) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s *checkpointMachine) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(s.stream), nil
}

func (s *checkpointMachine) Close(context.Context) error {
	s.closeCount += 1
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.closed {
		return nil
	}
	s.closed = true
	return s.stream.Close()
}

func (s *checkpointMachine) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *checkpointMachine) CreateSnapshot(_ context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	if s.snapshotHook != nil {
		s.snapshotHook()
	}
	s.snapshotRequests = append(s.snapshotRequests, request)
	if s.snapshotErr != nil {
		return vm.SnapshotArtifact{}, s.snapshotErr
	}
	return s.artifact, nil
}

func (*checkpointMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	return nil, errTestLiveCapture
}

func (s *checkpointMachine) Resume(context.Context) error {
	s.resumeCount += 1
	return nil
}

type checkpointCAS struct {
	mu              sync.Mutex
	putErrMediaType string
	puts            []checkpointCASPut
}

type checkpointCASPut struct {
	mediaType string
	content   []byte
	object    cas.Object
}

func (c *checkpointCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	return c.put(mediaType, content)
}

func (c *checkpointCAS) Stage(_ context.Context, mediaType string) (cas.Stage, error) {
	return &checkpointCASStage{store: c, mediaType: mediaType}, nil
}

func (c *checkpointCAS) put(mediaType string, content []byte) (cas.Object, error) {
	object := cas.Object{
		Digest:    sha256sum.DigestBytes(content),
		SizeBytes: int64(len(content)),
		MediaType: mediaType,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts = append(c.puts, checkpointCASPut{mediaType: mediaType, content: content, object: object})
	if c.putErrMediaType != "" && mediaType == c.putErrMediaType {
		return cas.Object{}, errors.New("put failed")
	}
	return object, nil
}

func checkpointPutByMediaType(t *testing.T, store *checkpointCAS, mediaType string) checkpointCASPut {
	t.Helper()
	for _, put := range store.puts {
		if put.mediaType == mediaType {
			return put
		}
	}
	t.Fatalf("missing checkpoint CAS put for media type %q: %+v", mediaType, store.puts)
	return checkpointCASPut{}
}

type checkpointCASStage struct {
	store     *checkpointCAS
	mediaType string
	content   bytes.Buffer
	closed    bool
}

func (s *checkpointCASStage) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("stage is closed")
	}
	return s.content.Write(p)
}

func (s *checkpointCASStage) Close() error {
	s.closed = true
	return nil
}

func (s *checkpointCASStage) Commit(context.Context) (cas.Object, error) {
	s.closed = true
	return s.store.put(s.mediaType, s.content.Bytes())
}

func (s *checkpointCASStage) Abort(context.Context) error {
	s.closed = true
	return nil
}

func (c *checkpointCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, nil
}

func (c *checkpointCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, put := range c.puts {
		if put.object.Digest == digest {
			return io.NopCloser(bytes.NewReader(append([]byte(nil), put.content...))), nil
		}
	}
	return nil, errors.New("not found")
}

func (c *checkpointCAS) Delete(context.Context, string) error {
	return nil
}

func checkpointArtifact(t *testing.T) vm.SnapshotArtifact {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state.vmstate")
	scratch := filepath.Join(dir, "scratch.ext4")
	memory := filepath.Join(dir, "memory.mem")
	if err := os.WriteFile(state, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, []byte("scratch"), 0o600); err != nil {
		t.Fatal(err)
	}
	computerPath := filepath.Join(dir, "computer.ext4")
	if err := os.WriteFile(computerPath, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	return vm.SnapshotArtifact{
		Computer:          &vm.ComputerSnapshot{ComputerID: "01912345-6789-7abc-8def-0123456789ab", Capture: &versionCaptureFixture{root: testVersionRoot(4096)}},
		RuntimeBackend:    "firecracker",
		RuntimeID:         "sha256:runtime",
		RuntimeArch:       "x86_64",
		VMRuntimeContract: "helmr.vm-runtime.v0",
		KernelDigest:      "sha256:kernel",
		InitramfsDigest:   "sha256:initramfs",
		RootfsDigest:      "sha256:rootfs",
		VMConfigDigest:    "sha256:runtime-config",
		VMVCPUCount:       2,
		CPUConfigDigest:   sha256sum.DigestBytes([]byte("cpu-config")),
		VMState:           vm.SnapshotFile{Path: state, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk: vm.SnapshotFile{Path: scratch, MediaType: cas.CheckpointScratchDiskMediaType, Filepack: &vm.FilepackStats{
			LogicalBytes:  1024,
			EncodedChunks: 1,
		}},
		Memory: []vm.SnapshotFile{{Path: memory, MediaType: cas.CheckpointMemoryMediaType}},
		Phases: []vm.Phase{{
			Name:      "pack_scratch_filepack",
			Role:      "scratch-disk",
			MediaType: cas.CheckpointScratchDiskMediaType,
			Filepack: &vm.FilepackStats{
				LogicalBytes:  1024,
				EncodedChunks: 1,
			},
		}},
		Manifest: []byte(`{"runtime":{"backend":"firecracker"}}`),
	}
}

func testCheckpointRuntimeArchitecture() string {
	return string(definition.ArchitectureX8664)
}

func checkpointPhaseHasFilepackStats(phases []workerapi.CheckpointPhase, name string) bool {
	for _, phase := range phases {
		if phase.Name == name && phase.Filepack != nil && phase.Filepack.LogicalBytes > 0 {
			return true
		}
	}
	return false
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s err = %v, want not exist", path, err)
	}
}

func (s *checkpointMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: 4096, MemoryBytes: 4096, ScratchBytes: 4096, StateBytes: 10000000, ConfigBytes: 65536}, nil
}

func testCheckpointReservations(t *testing.T) *reservation.Ledger {
	t.Helper()
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 1000, MemoryBytes: 1 << 30, GuestEphemeralDiskBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}
func (c *checkpointCAS) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, d.SizeBytes))
	if err != nil {
		return cas.Object{}, err
	}
	return c.put(d.MediaType, data)
}

func testCheckpointPublication(computerCheckpointRequest) disk.ContinuationPublication {
	return nil
}

type versionCaptureFixture struct {
	root     disk.VersionRoot
	publish  func(context.Context, disk.ContinuationPublication) error
	release  func()
	released bool
}

func (c *versionCaptureFixture) Root() disk.VersionRoot { return c.root }
func (c *versionCaptureFixture) Publish(ctx context.Context, p disk.ContinuationPublication) error {
	if c.released {
		return errors.New("capture already released")
	}
	if c.publish != nil {
		return c.publish(ctx, p)
	}
	return nil
}
func (c *versionCaptureFixture) Release() {
	if !c.released {
		c.released = true
		if c.release != nil {
			c.release()
		}
	}
}

func checkpointCaptureTarget(count int) workerapi.InstanceReconcileTarget {
	target := freezeTarget(count)
	target.Source.ComputerID = "01912345-6789-7abc-8def-0123456789ab"
	target.Source.VMPlatformID = "sha256:runtime"
	target.Source.VMVCPUCount = 2
	target.Source.CPUConfigDigest = sha256sum.DigestBytes([]byte("cpu-config"))
	return target
}
func checkpointFreezeStream(t *testing.T, target workerapi.InstanceReconcileTarget) *checkpointStream {
	t.Helper()
	response := &computerv0.FreezeComputerResponse{DesiredVersion: target.DesiredVersion, MembershipRevision: target.Capture.MembershipRevision, Identity: &computerv0.ComputerRestoreIdentity{ComputerId: target.Source.ComputerID, SourceComputerInstanceId: target.ID, WriterGeneration: target.Source.WriterGeneration, CheckpointId: target.Capture.CheckpointID}}
	for _, member := range target.Capture.Runs {
		response.Identity.Runs = append(response.Identity.Runs, &computerv0.CapturedRun{RunId: member.RunID, AttemptNumber: uint32(member.AttemptNumber), RunWaitId: member.RunWaitID, RunLeaseId: member.RunLeaseID, CorrelationId: "correlation-" + member.RunID})
	}
	return newCheckpointStream(t, nil, response)
}
func assertComputerFreezeFrame(t *testing.T, body []byte, target workerapi.InstanceReconcileTarget) {
	t.Helper()
	reader := bytes.NewReader(body)
	header, size, err := wire.ReadStreamFrameHeader(reader)
	if err != nil || size != 0 || header.Type != wire.StreamTypeComputerFreeze || header.ComputerID != target.Source.ComputerID || header.RunID != "" {
		t.Fatalf("invalid freeze header: %+v %v", header, err)
	}
	var request computerv0.FreezeComputerRequest
	if err := frameio.ReadProtoFrame(reader, &request); err != nil {
		t.Fatal(err)
	}
	if request.CheckpointId != target.Capture.CheckpointID || len(request.Runs) != len(target.Capture.Runs) {
		t.Fatalf("invalid membership: %v", &request)
	}
}
func TestComputerCheckpointerPreservesCaptureAndReleaseErrors(t *testing.T) {
	target := checkpointCaptureTarget(2)
	stream := checkpointFreezeStream(t, target)
	captureErr, releaseErr := errors.New("snapshot failed"), errors.New("stop failed")
	machine := &checkpointMachine{stream: stream, snapshotErr: captureErr, closeErr: releaseErr}
	_, err := (&computerCheckpointer{publication: testCheckpointPublication, machine: machine, objects: &checkpointCAS{}, reservations: testCheckpointReservations(t), encryptor: testCheckpointEncryptor(t), tempDir: t.TempDir()}).CreateCheckpoint(t.Context(), computerCheckpointRequest{Target: target, Register: func(context.Context, workerapi.CheckpointManifest) error { return nil }})
	var cleanup *SourceReleaseError
	if !errors.Is(err, captureErr) || !errors.Is(err, releaseErr) || !errors.As(err, &cleanup) {
		t.Fatalf("lost failure: %v", err)
	}
	if machine.closeCount != 1 || machine.resumeCount != 0 {
		t.Fatalf("close/resume = %d/%d", machine.closeCount, machine.resumeCount)
	}
}
func TestComputerCheckpointerRejectsMismatchedSnapshotBeforePublication(t *testing.T) {
	for _, field := range []string{"computer", "platform", "cpu-count", "cpu-config", "disk"} {
		t.Run(field, func(t *testing.T) {
			target := checkpointCaptureTarget(2)
			artifact := checkpointArtifact(t)
			switch field {
			case "computer":
				artifact.Computer.ComputerID = "other"
			case "platform":
				artifact.RuntimeID = "other"
			case "cpu-count":
				artifact.VMVCPUCount++
			case "cpu-config":
				artifact.CPUConfigDigest = sha256sum.DigestBytes([]byte("other"))
			case "disk":
				artifact.Computer = nil
			}
			machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: artifact}
			store := &checkpointCAS{}
			_, err := (&computerCheckpointer{publication: testCheckpointPublication, machine: machine, objects: store, reservations: testCheckpointReservations(t), encryptor: testCheckpointEncryptor(t), tempDir: t.TempDir()}).CreateCheckpoint(t.Context(), computerCheckpointRequest{Target: target, Register: func(context.Context, workerapi.CheckpointManifest) error {
				t.Fatal("registered changed snapshot")
				return nil
			}})
			if err == nil || machine.closeCount != 1 || len(store.puts) != 0 {
				t.Fatalf("unsafe capture: %v closes=%d puts=%d", err, machine.closeCount, len(store.puts))
			}
		})
	}
}
func TestComputerCheckpointerInvalidIntentDoesNotCloseUnrelatedSource(t *testing.T) {
	machine := &checkpointMachine{}
	_, err := (&computerCheckpointer{machine: machine}).CreateCheckpoint(t.Context(), computerCheckpointRequest{})
	if err == nil || machine.closeCount != 0 {
		t.Fatalf("err=%v closes=%d", err, machine.closeCount)
	}
}
func TestComputerCheckpointerConfigurationFailureClosesSource(t *testing.T) {
	machine := &checkpointMachine{stream: checkpointFreezeStream(t, checkpointCaptureTarget(0))}
	_, err := (&computerCheckpointer{machine: machine}).CreateCheckpoint(t.Context(), computerCheckpointRequest{Target: checkpointCaptureTarget(0)})
	if err == nil || machine.closeCount != 1 {
		t.Fatalf("err=%v closes=%d", err, machine.closeCount)
	}
}
