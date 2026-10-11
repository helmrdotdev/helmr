package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type checkpointPipelineClient struct {
	AllocatedComputerClient
	stepErr        error
	beginNotReady  bool
	t              *testing.T
	identity       workerapi.AllocationIdentity
	capture        []byte
	save           string
	failure        string
	failed         bool
	calls          []string
	manifests      []computercheckpoint.Manifest
	cancelled      *workerapi.AgentComputerUnsealedRequest
	cancelNotReady bool
	cancelAttempts []time.Time
	store          cas.Reader
	stopped        func(workerapi.AgentComputerStoppedRequest)
}

func (c *checkpointPipelineClient) step(name string) error {
	c.calls = append(c.calls, name)
	if name == c.failure && !c.failed {
		c.failed = true
		if c.stepErr != nil {
			return c.stepErr
		}
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (c *checkpointPipelineClient) BeginAgentComputerCapture(_ context.Context, r workerapi.AgentComputerCaptureRequest) (workerapi.AgentComputerCaptureResponse, error) {
	if c.beginNotReady {
		c.calls = append(c.calls, "begin")
		return workerapi.AgentComputerCaptureResponse{}, &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerNotReady}
	}
	if c.capture == nil {
		c.capture, _ = proto.Marshal(&agentv1.ComputerSessionCapture{CheckpointId: r.CheckpointID, DesiredVersion: 1, MembershipRevision: 1, Envelope: &computerv0.ComputerOperationEnvelope{OperationId: r.CheckpointID, ComputerId: r.ComputerID, ComputerInstanceId: c.identity.InstanceID, WriterGeneration: uint64(r.LeaseEpoch), ChannelCredential: r.ChannelCredential, OperationExpiresAtUnixNano: time.Now().Add(10 * time.Millisecond).UnixNano()}, Sessions: []*agentv1.SessionIdentity{{SessionId: uuid.NewV7().String(), ProcessEpoch: 1}}})
	}
	var capture agentv1.ComputerSessionCapture
	_ = proto.Unmarshal(c.capture, &capture)
	if r.CheckpointID != capture.CheckpointId {
		c.t.Fatal("retry changed capture identity")
	}
	return workerapi.AgentComputerCaptureResponse{Capture: c.capture, SaveID: c.save}, c.step("begin")
}
func (c *checkpointPipelineClient) SealAgentComputerCapture(context.Context, workerapi.AgentComputerReceiptRequest) error {
	return c.step("seal")
}
func (c *checkpointPipelineClient) CancelAgentComputerCapture(_ context.Context, r workerapi.AgentComputerUnsealedRequest) error {
	c.cancelled = &r
	c.cancelAttempts = append(c.cancelAttempts, r.AbsentObservedAt)
	if c.cancelNotReady && len(c.cancelAttempts) == 1 {
		return &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerNotReady}
	}
	return c.step("cancel")
}
func (c *checkpointPipelineClient) CaptureAgentSave(context.Context, workerapi.AgentSavePublication) error {
	return c.step("disk-capture")
}
func (c *checkpointPipelineClient) PublishAgentSave(context.Context, workerapi.AgentSavePublication) error {
	return c.step("disk-publish")
}
func (c *checkpointPipelineClient) RegisterAgentCheckpoint(_ context.Context, r workerapi.AgentCheckpointPublication) error {
	c.manifests = append(c.manifests, r.Manifest)
	return c.step("register")
}
func (c *checkpointPipelineClient) CompleteAgentCheckpoint(ctx context.Context, r workerapi.AgentCheckpointPublication) error {
	c.manifests = append(c.manifests, r.Manifest)
	for _, object := range r.Manifest.Objects() {
		got, err := c.store.Stat(ctx, object.Digest)
		if err != nil {
			return err
		}
		if err = cas.RequireExact(got, cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}); err != nil {
			return err
		}
	}
	return c.step("complete")
}

type checkpointPipelineMachine struct {
	vm.CheckpointableMachine
	control                    *agentControlMachine
	artifact                   vm.SnapshotArtifact
	mode                       string
	snapshots, resumes, aborts int
}

func (m *checkpointPipelineMachine) BeginCheckpoint(context.Context, vm.SnapshotRequest) (vm.CheckpointCapture, error) {
	return m, nil
}
func (m *checkpointPipelineMachine) PrepareGuest(ctx context.Context, exchange func(context.Context, vm.Stream) error) error {
	s, err := m.control.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	return exchange(ctx, s)
}
func (m *checkpointPipelineMachine) OpenStream(ctx context.Context) (vm.Stream, error) {
	return m.control.OpenStream(ctx)
}
func (m *checkpointPipelineMachine) WithRunningGuestControl(ctx context.Context, _ vm.GuestControlStage, run func(context.Context) error) error {
	return run(ctx)
}
func (m *checkpointPipelineMachine) CreateSnapshot(context.Context) (vm.SnapshotArtifact, error) {
	m.snapshots++
	if m.mode == "cut-lost" {
		return m.artifact, io.ErrUnexpectedEOF
	}
	return m.artifact, nil
}
func (m *checkpointPipelineMachine) ResumeGuestControl(context.Context) error {
	m.resumes++
	return nil
}
func (m *checkpointPipelineMachine) CompleteAbort(context.Context) error { m.aborts++; return nil }
func (m *checkpointPipelineMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: 4096, MemoryBytes: 4096, ScratchBytes: 4096, StateBytes: 4096, ConfigBytes: 4096}, nil
}

func checkpointPipelineFixture(t *testing.T, mode, failure string) (*ComputerAllocationOwner, *checkpointPipelineClient, *checkpointPipelineMachine, *saveHostFixture) {
	t.Helper()
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c := &checkpointPipelineClient{t: t, identity: identity, save: uuid.NewV7().String(), failure: failure, store: store}
	cut := &saveHostFixture{computer: identity.OwnerID, root: testVersionRoot(4096)}
	m := &checkpointPipelineMachine{mode: mode, artifact: vm.SnapshotArtifact{Computer: &vm.ComputerSnapshot{ComputerID: identity.OwnerID, Capture: saveHostCapture{cut}}, Manifest: []byte(`{"runtime":"test"}`), RuntimeID: "test", VMVCPUCount: 1}}
	m.artifact.VMConfigDigest = sha256sum.DigestBytes(m.artifact.Manifest)
	dir := t.TempDir()
	for _, object := range []struct {
		role, media string
		target      *vm.SnapshotFile
	}{{"state", cas.CheckpointVMStateMediaType, &m.artifact.VMState}, {"scratch", cas.CheckpointScratchDiskMediaType, &m.artifact.ScratchDisk}} {
		path := filepath.Join(dir, object.role)
		if err = os.WriteFile(path, []byte(object.role), 0600); err != nil {
			t.Fatal(err)
		}
		*object.target = vm.SnapshotFile{Path: path, MediaType: object.media}
	}
	memory := filepath.Join(dir, "memory")
	if err = os.WriteFile(memory, []byte("native-memory-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	m.artifact.Memory = []vm.SnapshotFile{{Path: memory, MediaType: cas.CheckpointMemoryMediaType}}
	m.control = &agentControlMachine{handle: func(stream net.Conn) {
		if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
			return
		}
		var request agentv1.ComputerSessionControl
		if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request); err != nil {
			return
		}
		if mode == "lost-unsealed" && request.GetCapture() != nil {
			return
		}
		var receipt *agentv1.ComputerSessionReceipt
		if mode == "rejected" || mode == "lost-unsealed" {
			receipt = &agentv1.ComputerSessionReceipt{Error: "no retained record"}
			if request.GetInspect() != nil {
				receipt.ErrorCode = agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY
			}
		} else {
			capture := request.GetCapture()
			receipt = &agentv1.ComputerSessionReceipt{CheckpointId: capture.GetCheckpointId(), DesiredVersion: capture.GetDesiredVersion(), Frozen: true}
		}
		_ = frameio.WriteProtoFrame(stream, receipt)
	}}
	o := &ComputerAllocationOwner{identity: identity, client: c, machine: m, machines: &PreparedMachines{CheckpointCipher: cipher, ComputerObjects: saveStoragePublisher{store}, TempDir: t.TempDir()}}
	return o, c, m, cut
}

func TestComputerCheckpointPipelineRetainsOneCutThroughPublicationUncertainty(t *testing.T) {
	for _, failure := range []string{"", "begin", "seal", "disk-capture", "disk-publish", "register", "complete"} {
		t.Run(failure, func(t *testing.T) {
			o, c, m, cut := checkpointPipelineFixture(t, "", failure)
			suspended, resumed := 0, 0
			err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { suspended++; return func() { resumed++ } })
			if !errors.Is(err, errComputerHibernated) {
				t.Fatal(err)
			}
			if m.snapshots != 1 || suspended != 1 || resumed != 0 || m.resumes != 0 || m.aborts != 0 || o.checkpointCut == nil {
				t.Fatalf("physical ownership changed: %#v", m)
			}
			if !reflect.DeepEqual(cut.steps, []string{"objects"}) {
				t.Fatalf("cut released or repeated before physical cleanup: %v", cut.steps)
			}
			for _, manifest := range c.manifests {
				if !reflect.DeepEqual(manifest, c.manifests[0]) {
					t.Fatal("publication retry changed encrypted bytes")
				}
			}
			manifest := c.manifests[0]
			path, err := downloadCheckpointObject(t.Context(), c.store, o.machines.CheckpointCipher, t.TempDir(), manifest.CheckpointID.String(), computercheckpoint.RuntimeObject{Role: "memory", Object: manifest.Memory}, 4096)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "native-memory-sentinel" {
				t.Fatalf("roundtrip %q %v", data, err)
			}
			o.checkpointCut.Release()
		})
	}
}
func TestComputerCheckpointUnsealedCaptureResumesWithoutSerialization(t *testing.T) {
	for _, mode := range []string{"rejected", "lost-unsealed"} {
		t.Run(mode, func(t *testing.T) {
			o, c, m, _ := checkpointPipelineFixture(t, mode, "")
			err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} })
			if err != nil {
				t.Fatal(err)
			}
			if m.snapshots != 0 || m.resumes != 1 || m.aborts != 1 || c.cancelled == nil {
				t.Fatalf("unsafe unsealed outcome: %+v", c.cancelled)
			}
			if (mode == "rejected") != c.cancelled.Rejected {
				t.Fatal("uncertainty fabricated rejection")
			}
			if mode == "lost-unsealed" && c.cancelled.AbsentObservedAt.IsZero() {
				t.Fatal("missing fresh absence")
			}
		})
	}
}
func TestComputerCheckpointFailedCutCannotBecomeNoCutEvidence(t *testing.T) {
	o, c, m, _ := checkpointPipelineFixture(t, "cut-lost", "")
	err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} })
	if !errors.Is(err, io.ErrUnexpectedEOF) || m.snapshots != 1 || o.checkpointCut == nil || c.cancelled != nil || m.aborts != 0 {
		t.Fatalf("cut uncertainty lost: %v", err)
	}
	o.checkpointCut.Release()
}

type checkpointRestoreBackend struct {
	vm.Backend
	t              *testing.T
	calls          int
	identity       workerapi.AllocationIdentity
	machine        vm.CheckpointableMachine
	verifyFilepack bool
	restoreErr     error
}

func (b *checkpointRestoreBackend) Restore(_ context.Context, r vm.RestoreRequest) (vm.CheckpointableMachine, error) {
	b.calls++
	if r.ComputerInstanceID != b.identity.InstanceID || r.Binding.ComputerInstanceID != b.identity.InstanceID || r.Topology.Computer.ComputerID != b.identity.OwnerID || len(r.Memory) != 1 {
		b.t.Fatal("restore lost target ownership")
	}
	data, err := os.ReadFile(r.Memory[0])
	if err != nil || string(data) != "native-memory-sentinel" {
		b.t.Fatalf("backend received non-decrypted memory %q %v", data, err)
	}
	if b.verifyFilepack {
		_, err := filepack.VerifyFrom(context.Background(), bytes.NewReader(data), filepack.MemoryRole, 4096)
		return nil, err
	}
	if b.restoreErr != nil {
		return nil, b.restoreErr
	}
	return b.machine, nil
}
func (c *checkpointPipelineClient) ObserveAgentComputerStopped(_ context.Context, request workerapi.AgentComputerStoppedRequest) error {
	if c.stopped != nil {
		c.stopped(request)
	}
	return nil
}
func (b *checkpointRestoreBackend) Cleanup(_ context.Context, owner vm.Owner) error {
	if owner.ID != b.identity.InstanceID {
		b.t.Fatal("cleanup changed owner")
	}
	return nil
}
func (c *checkpointPipelineClient) ReadAgentCheckpoint(context.Context, workerapi.AllocationIdentity) (computercheckpoint.Manifest, error) {
	return c.manifests[0], nil
}

type corruptCheckpointStore struct {
	cas.ImmutableStore
	digest string
}

func (s corruptCheckpointStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	if digest == s.digest {
		return io.NopCloser(bytes.NewReader([]byte("corrupt"))), nil
	}
	return s.ImmutableStore.Get(ctx, digest)
}

func TestComputerCheckpointRestoreAuthenticatesBeforePhysicalCreation(t *testing.T) {
	for _, fault := range []string{"none", "wrong-computer", "wrong-disk", "wrong-runtime", "wrong-vcpu", "corrupt-memory", "wrong-key", "invalid-filepack", "backend-io"} {
		t.Run(fault, func(t *testing.T) {
			source, c, _, _ := checkpointPipelineFixture(t, "", "")
			if err := source.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} }); !errors.Is(err, errComputerHibernated) {
				t.Fatal(err)
			}
			defer source.checkpointCut.Release()
			id := source.identity
			id.InstanceID = uuid.NewV7().String()
			id.Epoch++
			m := c.manifests[0]
			b := &checkpointRestoreBackend{t: t, identity: id, machine: &checkpointPipelineMachine{}}
			paused := false
			target := &ComputerAllocationOwner{identity: id, client: c, checkpointKeyUnavailable: func() { paused = true }, machines: &PreparedMachines{Backend: b, CheckpointCipher: source.machines.CheckpointCipher, ComputerObjects: source.machines.ComputerObjects, TempDir: t.TempDir()}}
			delivery := workerapi.ComputerAllocationDelivery{Identity: id, BaseVersion: c.save, RestoredFrom: c.save, Shape: workerapi.AllocationShape{VMPlatformID: m.Runtime.RuntimeID, VCPUCount: 1, MemoryBytes: 4096, ScratchBytes: 4096}}
			material := workerapi.ComputerAllocationSource{Disk: workerapi.ComputerSourceMaterial{Root: m.Disk}}
			switch fault {
			case "wrong-computer":
				c.manifests[0].ComputerID = uuid.NewV7()
			case "wrong-disk":
				material.Disk.Root = testVersionRoot(8192)
			case "wrong-runtime":
				delivery.Shape.VMPlatformID = "other"
			case "wrong-vcpu":
				delivery.Shape.VCPUCount = 2
			case "corrupt-memory":
				target.machines.ComputerObjects = corruptCheckpointStore{ImmutableStore: target.machines.ComputerObjects, digest: m.Memory.Digest}
			case "invalid-filepack":
				b.verifyFilepack = true
			case "backend-io":
				b.restoreErr = syscall.EIO
			case "wrong-key":
				target.machines.CheckpointCipher, _ = NewCheckpointEncryptor(bytes.Repeat([]byte{4}, 32))
			}
			err := target.restore(t.Context(), delivery, material, nil)
			if paused != (fault == "wrong-key") || errors.Is(err, ErrCheckpointKeyUnavailable) != (fault == "wrong-key") {
				t.Fatalf("incorrect key fault classification: paused=%v error=%v", paused, err)
			}

			if (target.invalidCheckpointID != "") != (fault == "corrupt-memory" || fault == "invalid-filepack") {
				t.Fatalf("incorrect invalid checkpoint report: %q", target.invalidCheckpointID)
			}
			if fault == "wrong-key" || fault == "corrupt-memory" || fault == "invalid-filepack" {
				stopped := false
				c.stopped = func(request workerapi.AgentComputerStoppedRequest) {
					stopped = true
					if fault == "wrong-key" && !paused {
						t.Fatal("stop report preceded admission pause")
					}
					if request.InvalidCheckpointID != target.invalidCheckpointID {
						t.Fatal("physical stop lost exact failure classification")
					}
				}
				if err := target.cleanup(); err != nil || !stopped || !target.finished.Load() {
					t.Fatalf("failed target did not finish cleanup: %v", err)
				}
				if fault == "wrong-key" {
					replacement := &ComputerAllocationOwner{identity: id, client: c, machines: &PreparedMachines{Backend: b, CheckpointCipher: source.machines.CheckpointCipher, ComputerObjects: source.machines.ComputerObjects, TempDir: t.TempDir()}}
					if err := replacement.restore(t.Context(), delivery, material, nil); err != nil || replacement.checkpoint == nil || b.calls != 1 {
						t.Fatalf("correct key failed ordinary retained restore: %v", err)
					}
					b.calls = 0
				}
			}
			if fault == "none" {
				if err != nil || b.calls != 1 || target.checkpoint == nil {
					t.Fatalf("restore %v calls=%d", err, b.calls)
				}
			} else {
				wantCalls := 0
				if fault == "invalid-filepack" || fault == "backend-io" {
					wantCalls = 1
				}
				if err == nil || b.calls != wantCalls {
					t.Fatalf("restore boundary: %v calls=%d want=%d", err, b.calls, wantCalls)
				}
			}
		})
	}
}

func TestComputerCheckpointRetainsExpiryObservationWhileDatabaseClockCatchesUp(t *testing.T) {
	o, c, m, _ := checkpointPipelineFixture(t, "lost-unsealed", "")
	c.cancelNotReady = true
	if err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} }); err != nil {
		t.Fatal(err)
	}
	if len(c.cancelAttempts) != 2 || !c.cancelAttempts[0].Equal(c.cancelAttempts[1]) || c.cancelAttempts[0].IsZero() || m.snapshots != 0 || m.aborts != 1 {
		t.Fatalf("lost exact expiry evidence: %+v", c.cancelAttempts)
	}
}

func TestComputerCheckpointUnsuccessfulQualificationBacksOff(t *testing.T) {
	for _, kind := range []string{"busy", "guest-rejected"} {
		t.Run(kind, func(t *testing.T) {
			o, c, m, _ := checkpointPipelineFixture(t, "rejected", "")
			c.beginNotReady = kind == "busy"
			call := func() {
				t.Helper()
				if err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() {} }); err != nil {
					t.Fatal(err)
				}
			}
			call()
			firstCalls, firstResumes := len(c.calls), m.resumes
			for range 20 {
				call()
			}
			if len(c.calls) != firstCalls || m.resumes != firstResumes || (kind == "guest-rejected" && o.checkpointRetryDelay != time.Second) || (kind == "busy" && o.checkpointRetryDelay != 0) {
				t.Fatal("failed qualification was polled or disturbed guest during backoff")
			}
			// Advance the retry deadline without waiting in the test.
			c.capture = nil
			o.checkpointRetryAt = time.Time{}
			call()
			if len(c.calls) == firstCalls || (kind == "guest-rejected" && o.checkpointRetryDelay != 2*time.Second) || (kind == "busy" && o.checkpointRetryDelay != 0) {
				t.Fatal("backoff did not retry and increase")
			}
		})
	}
	if checkpointRetryDelay(30*time.Second) != 30*time.Second {
		t.Fatal("unbounded retry delay")
	}
}

type checkpointFaultReader struct {
	source    io.Reader
	remaining int
	failed    bool
}

func (r *checkpointFaultReader) Read(p []byte) (int, error) {
	if !r.failed && r.remaining == 0 {
		r.failed = true
		return 0, syscall.ENOSPC
	}
	r.remaining--
	return r.source.Read(p)
}

type checkpointFaultPublisher struct {
	cas.ImmutableStore
	failed bool
	digest string
}

func (p *checkpointFaultPublisher) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	if !p.failed {
		p.failed = true
		p.digest = d.Digest
		return cas.Object{}, syscall.EIO
	}
	if p.digest != "" {
		if d.Digest != p.digest {
			return cas.Object{}, errors.New("retry changed immutable object")
		}
		p.digest = ""
	}
	return p.ImmutableStore.Publish(ctx, d, f)
}
func TestComputerCheckpointPreservesSourceThroughDurabilityFailures(t *testing.T) {
	for _, kind := range []string{"disk-conflict", "encryption-space", "object-io"} {
		t.Run(kind, func(t *testing.T) {
			o, c, m, cut := checkpointPipelineFixture(t, "", "")
			switch kind {
			case "disk-conflict":
				c.failure = "disk-publish"
				c.stepErr = &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerNotReady}
			case "encryption-space":
				o.machines.CheckpointCipher.rand = &checkpointFaultReader{source: o.machines.CheckpointCipher.rand, remaining: 2}
			case "object-io":
				o.machines.ComputerObjects = &checkpointFaultPublisher{ImmutableStore: o.machines.ComputerObjects}
			}
			err := o.captureIdle(t.Context(), workerapi.ComputerAllocationDelivery{ChannelCredential: "private"}, func() func() { return func() { t.Error("resumed before durable publication") } })
			if !errors.Is(err, errComputerHibernated) || m.snapshots != 1 || m.aborts != 0 || c.cancelled != nil || !reflect.DeepEqual(cut.steps, []string{"objects"}) {
				t.Fatalf("healthy source lost or recut: %v steps=%v", err, cut.steps)
			}
			dirs, err := os.ReadDir(filepath.Join(o.machines.computerPreparationDirectory(o.identity.InstanceID, o.identity.Epoch), "checkpoint"))
			if err != nil || len(dirs) != 1 {
				t.Fatalf("encryption retry leaked staging: dirs=%d error=%v", len(dirs), err)
			}
			o.checkpointCut.Release()
		})
	}
}
func TestComputerCheckpointDurabilityEndsAtAuthorityLoss(t *testing.T) {
	o := &ComputerAllocationOwner{}
	want := &httpclient.Error{StatusCode: 401}
	calls := 0
	if err := o.retryCheckpointDurability(t.Context(), func(context.Context) error { calls++; return want }); err != want || calls != 1 {
		t.Fatalf("authority rejection retried: %v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("physical lease expired")
	cancel(cause)
	if err := o.retryCheckpointDurability(ctx, func(context.Context) error { t.Fatal("publication after lease loss"); return nil }); !errors.Is(err, cause) {
		t.Fatalf("lease cause lost: %v", err)
	}
}

func TestComputerCheckpointPermanentDurabilityRejectionsDoNotLoop(t *testing.T) {
	for _, err := range []error{&httpclient.Error{StatusCode: 400}, &httpclient.Error{StatusCode: 409}, &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerEvidenceConflict}, computercheckpoint.ErrInvalidManifest, errors.New("plaintext exceeds bound")} {
		o := &ComputerAllocationOwner{}
		calls := 0
		got := o.retryCheckpointDurability(t.Context(), func(context.Context) error { calls++; return err })
		if got != err || calls != 1 {
			t.Fatalf("permanent invalid capture retried: %v calls=%d", got, calls)
		}
	}
}
func TestComputerCheckpointUnownedInspectionDoesNotLoop(t *testing.T) {
	o, c, m, _ := checkpointPipelineFixture(t, "rejected", "")
	// No capture exchange is issued: the fake's generic negative is not the
	// typed pending-admission response that authorizes waiting.
	m.control = &agentControlMachine{handle: func(stream net.Conn) {
		if _, _, err := wire.ReadStreamFrameHeader(stream); err != nil {
			return
		}
		var request agentv1.ComputerSessionControl
		if frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &request) != nil {
			return
		}
		_ = frameio.WriteProtoFrame(stream, &agentv1.ComputerSessionReceipt{Error: "mounted owner unavailable"})
	}}
	capture := &agentv1.ComputerSessionCapture{CheckpointId: uuid.NewV7().String()}
	if err := o.abortUncutCheckpoint(t.Context(), workerapi.ComputerAllocationDelivery{}, capture, m); err == nil || c.cancelled != nil || m.aborts != 0 {
		t.Fatalf("unowned inspection treated as absence: %v", err)
	}
}
