package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type preparationOwnerClient struct {
	deliveries, renewals          atomic.Int64
	appendLog                     func(context.Context, workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error)
	loseWriteKey                  atomic.Bool
	keyCalls                      atomic.Int64
	delivery                      workerapi.PreparationAllocationDelivery
	start                         workerapi.PreparationStart
	backend                       *preparationOwnerBackend
	writeKey                      atomic.Bool
	sealed                        atomic.Bool
	publications, stops, failures atomic.Int64
	losePublish, loseStop         atomic.Bool
	root                          disk.VersionRoot
}

func (c *preparationOwnerClient) AppendPreparationLog(ctx context.Context, request workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error) {
	if c.appendLog != nil {
		return c.appendLog(ctx, request)
	}
	now := time.Now()
	return workerapi.DiagnosticLogReceipt{ThroughSequence: request.ThroughSequence, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}, nil
}
func (c *preparationOwnerClient) DeliverPreparationAllocation(context.Context, workerapi.AllocationIdentity) (workerapi.PreparationAllocationDelivery, error) {
	c.deliveries.Add(1)
	d := c.delivery
	d.ChannelCredential = append([]byte(nil), d.ChannelCredential...)
	return d, nil
}
func (c *preparationOwnerClient) PreparationStart(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationStart, error) {
	return c.start, nil
}
func (c *preparationOwnerClient) PreparationSecrets(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationSecrets, error) {
	return workerapi.PreparationSecrets{Secrets: []workerapi.SecretDelivery{
		{Env: &workerapi.SecretEnv{Name: "BUILD"}, Value: []byte("secret")},
		{File: &workerapi.SecretFile{Path: "/etc/build/token"}, Value: []byte("file-secret")},
	}, Protected: &workerapi.ProtectedEnv{Env: map[string]string{"PROTECTED": "placeholder"}, CA: []byte("public trust")}}, nil
}
func (c *preparationOwnerClient) PreparationWriteKey(context.Context, workerapi.PreparationExecutor) (workerapi.ComputerKeyMaterial, error) {
	c.keyCalls.Add(1)
	if c.loseWriteKey.Swap(false) {
		return workerapi.ComputerKeyMaterial{}, errors.New("key reply lost")
	}
	c.writeKey.Store(true)
	return workerapi.ComputerKeyMaterial{Scope: "scope", ID: "01900000-0000-7000-8000-000000000001", Key: bytes.Repeat([]byte{15}, 32)}, nil
}
func (c *preparationOwnerClient) RenewPreparation(context.Context, workerapi.PreparationExecutor) (workerapi.PreparationRenewal, error) {
	c.renewals.Add(1)
	return workerapi.PreparationRenewal{ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (c *preparationOwnerClient) FailPreparation(context.Context, workerapi.PreparationFailure) error {
	c.failures.Add(1)
	return nil
}
func (c *preparationOwnerClient) BeginPreparationCapture(context.Context, workerapi.PreparationCaptureBegin) error {
	if !c.writeKey.Load() {
		return errors.New("write key not pinned")
	}
	c.sealed.Store(true)
	return nil
}
func (c *preparationOwnerClient) RegisterPreparationObject(context.Context, workerapi.PreparationObject) error {
	if !c.sealed.Load() || !c.backend.absent.Load() {
		return errors.New("object registered before seal or physical stop")
	}
	return nil
}
func (c *preparationOwnerClient) CertifyPreparationObject(context.Context, workerapi.PreparationObject) error {
	return nil
}
func (c *preparationOwnerClient) RecordPreparationCapture(_ context.Context, r workerapi.PreparationPublication) error {
	c.root = r.Root
	return nil
}
func (c *preparationOwnerClient) PublishPreparation(_ context.Context, r workerapi.PreparationPublication) error {
	if r.Root != c.root {
		return errors.New("capture root changed")
	}
	c.publications.Add(1)
	if c.losePublish.Swap(false) {
		return errors.New("publication reply lost")
	}
	return nil
}
func (c *preparationOwnerClient) ObservePreparationStopped(context.Context, workerapi.AllocationIdentity) error {
	c.stops.Add(1)
	if c.loseStop.Swap(false) {
		return errors.New("stop reply lost")
	}
	return nil
}

type preparationOwnerStore struct{ *cas.File }

func (s preparationOwnerStore) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	if err := cas.VerifyDescriptorFile(ctx, d, file); err != nil {
		return cas.Object{}, err
	}
	return s.Put(ctx, d.MediaType, io.NewSectionReader(file, 0, d.SizeBytes))
}

type preparationOwnerBackend struct {
	unsupportedMachineStarts
	machine                          *allocationOwnerTestMachine
	starts, authored, flushes        atomic.Int64
	failCleanup, absent, guestFailed atomic.Bool
}

func (b *preparationOwnerBackend) Materialize(_ context.Context, r vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	if r.Topology.Computer == nil || r.Topology.Computer.File == nil || r.Topology.Computer.Device != nil || len(r.ReadOnlyDrives) != 2 {
		return nil, errors.New("invalid preparation VM topology")
	}
	b.starts.Add(1)
	return b.machine, nil
}
func (b *preparationOwnerBackend) Cleanup(context.Context, vm.Owner) error {
	if b.failCleanup.Load() {
		return errors.New("physical absence unproved")
	}
	b.absent.Store(true)
	return nil
}

func preparationOwnerFixture(t *testing.T) (*PreparationAllocationOwner, *preparationOwnerClient, *preparationOwnerBackend) {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := workerapi.AllocationIdentity{Kind: "preparation", EnvironmentID: uuid.NewV7().String(), OwnerID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 1}
	digest := sha256sum.DigestBytes([]byte("seed"))
	backend := &preparationOwnerBackend{}
	client := &preparationOwnerClient{backend: backend, delivery: workerapi.PreparationAllocationDelivery{Identity: identity, Shape: workerapi.AllocationShape{CPUMillis: 1000, VCPUCount: 1, MemoryBytes: 512 << 20, ScratchBytes: 1 << 30, VMPlatformID: "platform", CPUConfigDigest: "shape"}, ExpiresAt: time.Now().Add(time.Minute), ChannelCredential: bytes.Repeat([]byte{23}, 32)}, start: workerapi.PreparationStart{ComputerDefinitionID: "repo", RootfsDigest: digest, Seed: definition.ComputerSeedManifest{Profile: definition.ComputerSeedProfile, ArtifactDigest: digest, MediaType: definition.ComputerSeedMediaType}, SeedObject: workerapi.CASObject{Digest: digest, SizeBytes: 4096, MediaType: definition.ComputerSeedMediaType}, Program: workerapi.RuntimeProgram{Runtime: workerapi.CASObject{SizeBytes: 4096}, Artifact: workerapi.CASObject{SizeBytes: 4096}}}}
	backend.machine = &allocationOwnerTestMachine{agentControlMachine: &agentControlMachine{handle: func(stream net.Conn) {
		header, _, err := wire.ReadStreamFrameHeader(stream)
		if err != nil {
			// A failed executor cancels concurrent probes, including a transport
			// opened immediately before cancellation but not yet written to.
			if !errors.Is(err, io.EOF) {
				t.Error(err)
			}
			return
		}
		switch header.Type {
		case wire.StreamTypeComputerRuntimePrepare:
			var r computerv0.PrepareComputerRuntimeRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				t.Error(err)
				return
			}
			_ = frameio.WriteProtoFrame(stream, &computerv0.PrepareComputerRuntimeResponse{Status: "prepared", ComputerInstanceId: r.ComputerInstanceId})
		case wire.StreamTypeComputerMaterialize:
			var r computerv0.MaterializeComputerRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				t.Error(err)
				return
			}
			_ = frameio.WriteProtoFrame(stream, &computerv0.MaterializeComputerResponse{Status: "running", Target: r.Target, GuestChannelCredentialHash: sha256sum.HexBytes([]byte(r.Envelope.ChannelCredential))})
		case wire.StreamTypePreparationControl:
			var r computerv0.PreparationControlRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				// Joining lifecycle cancellation may close an empty probe stream.
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				return
			}
			if r.Start != nil {
				if len(r.Start.Secrets) != 2 || r.Start.Secrets[1].PlacementKind != "file" || r.Start.Secrets[1].PlacementTarget != "/etc/build/token" || string(r.Start.Secrets[1].Value) != "file-secret" || r.Start.ProtectedEnv["PROTECTED"] != "placeholder" || string(r.Start.ProxyCa) != "public trust" {
					t.Error("preparation placement or public trust was lost")
				}
				backend.authored.Add(1)
				return
			} // The first authored-start reply is lost.
			state := "absent"
			if backend.authored.Load() > 0 {
				state = "succeeded"
				if backend.guestFailed.Load() {
					state = "failed"
				}
			}
			_ = frameio.WriteProtoFrame(stream, &computerv0.PreparationControlResponse{State: state})
		case wire.StreamTypeComputerFlush:
			var r computerv0.FlushComputerRequest
			if err := frameio.ReadProtoFrame(stream, &r); err != nil {
				t.Error(err)
				return
			}
			backend.flushes.Add(1)
			_ = frameio.WriteProtoFrame(stream, &computerv0.FlushComputerResponse{OperationId: r.OperationId})
		default:
			t.Errorf("unexpected preparation stream %s", header.Type)
		}
	}}}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 8000, MemoryBytes: 8 << 30, HostDiskBytes: 128 << 30, VMSlots: 8})
	if err != nil {
		t.Fatal(err)
	}
	machines := &PreparedMachines{PreparationLogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, Backend: backend, CAS: store, PlatformStore: store, ComputerObjects: preparationOwnerStore{store}, Reservations: ledger, TempDir: t.TempDir(), RuntimeArchitecture: definition.ArchitectureX8664, ComputerStagingBytes: 4 << 20}
	owner, err := NewPreparationAllocationOwner(machines, client, identity, 1, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	owner.stageImage = func(_ context.Context, stage *preparationStage, _ workerapi.AllocationIdentity, _ workerapi.PreparationStart) error {
		var err error
		stage.disk, err = os.Create(filepath.Join(stage.directory, "image.raw"))
		if err != nil {
			return err
		}
		if err := stage.disk.Truncate(disk.SeedCapacity); err != nil {
			return err
		}
		stage.drives = []vm.ReadOnlyDrive{{ID: vm.ProgramRuntimeDrive}, {ID: vm.ProgramDrive}}
		_, err = stage.disk.WriteAt(bytes.Repeat([]byte{31}, 4096), 4096)
		return err
	}
	return owner, client, backend
}

func TestPreparationAllocationReconcilesLostStartPublishAndStop(t *testing.T) {
	owner, client, backend := preparationOwnerFixture(t)
	client.losePublish.Store(true)
	client.loseWriteKey.Store(true)
	client.loseStop.Store(true)
	if err := owner.Run(t.Context()); err == nil || owner.Finished() {
		t.Fatal("lost stop reply was hidden")
	} else {
		t.Logf("first operation outcome: %v", err)
	}
	if client.keyCalls.Load() != 2 || client.publications.Load() != 2 || backend.starts.Load() != 1 || backend.authored.Load() != 1 || backend.flushes.Load() != 1 || client.failures.Load() != 0 {
		t.Fatalf("ordering: publications=%d starts=%d authored=%d flushes=%d failures=%d", client.publications.Load(), backend.starts.Load(), backend.authored.Load(), backend.flushes.Load(), client.failures.Load())
	}
	owner.admitStart = func(context.Context) error {
		t.Fatal("cleanup invoked startup admission")
		return errors.New("unhealthy")
	}
	if err := owner.Run(t.Context()); err != nil || !owner.Finished() {
		t.Fatalf("stop reconciliation: %v", err)
	}
	if backend.starts.Load() != 1 || backend.authored.Load() != 1 || len(owner.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("cleanup restarted or retained released capacity")
	}
}
func TestPreparationAllocationRetainsResourcesUntilPhysicalAbsence(t *testing.T) {
	owner, client, backend := preparationOwnerFixture(t)
	backend.guestFailed.Store(true)
	backend.failCleanup.Store(true)
	backend.machine.failClose.Store(true)
	if err := owner.Run(t.Context()); err == nil {
		t.Fatal("failed execution and cleanup hidden")
	}
	if owner.Finished() || client.stops.Load() != 0 || len(owner.machines.Reservations.Snapshot().Reservations) != 1 {
		t.Fatal("unproved physical cleanup released custody")
	}
	directory := owner.stage.directory
	if _, err := os.Stat(directory); err != nil {
		t.Fatal("live VM staging released")
	}
	backend.failCleanup.Store(false)
	if err := owner.Run(t.Context()); err != nil || !owner.Finished() {
		t.Fatalf("backend reconciliation: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stopped VM staging retained")
	}
	if backend.authored.Load() != 1 || backend.starts.Load() != 1 || client.publications.Load() != 0 {
		t.Fatal("failed preparation was retried or published")
	}
}

func TestPreparationOutputOutageDoesNotBlockPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, client, backend := preparationOwnerFixture(t)
		fallback := backend.machine.handle
		var expires, renewals, attempts atomic.Int64
		var expired atomic.Bool
		startedAt := time.Now()
		backend.machine.handle = func(stream net.Conn) {
			raw := new(bytes.Buffer)
			header, _, err := wire.ReadStreamFrameHeader(io.TeeReader(stream, raw))
			if err != nil {
				// Publication can cancel a diagnostic poll before its header is sent.
				if !errors.Is(err, io.EOF) {
					t.Error(err)
				}
				return
			}
			if header.Type != wire.StreamTypePreparationControl {
				fallback(&preparationReplayConn{Conn: stream, reader: io.MultiReader(bytes.NewReader(raw.Bytes()), stream)})
				return
			}
			var request computerv0.PreparationControlRequest
			if err := frameio.ReadProtoFrame(stream, &request); err != nil {
				// Publication may cancel the poll after its header but before its request.
				if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
				return
			}
			if request.Start != nil {
				backend.authored.Add(1)
				expires.Store(request.ExpiresAtUnixNano)
			}
			response := &computerv0.PreparationControlResponse{State: "absent"}
			if backend.authored.Load() != 0 {
				if !time.Unix(0, expires.Load()).After(time.Now()) {
					expired.Store(true)
				}
				if request.ExpiresAtUnixNano > expires.Load() {
					expires.Store(request.ExpiresAtUnixNano)
				}
				response.State = "running"
				if request.RenewOnly {
					renewals.Add(1)
				}
				if request.LogStream != "" {
					response.Log = &computerv0.PreparationLog{Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: startedAt.UnixNano(), Data: []byte("build output")}
				}
				if time.Since(startedAt) >= 70*time.Second {
					response.State = "succeeded"
				}
				if expired.Load() {
					response.State = "failed"
				}
			}
			_ = frameio.WriteProtoFrame(stream, response)
		}
		client.appendLog = func(ctx context.Context, request workerapi.PreparationLogRequest) (workerapi.DiagnosticLogReceipt, error) {
			attempts.Add(1)
			// An indefinitely unavailable sink honors the bounded call's cancellation.
			<-ctx.Done()
			return workerapi.DiagnosticLogReceipt{}, ctx.Err()
		}
		if err := owner.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if expired.Load() || renewals.Load() < 2 || attempts.Load() < 2 || backend.authored.Load() != 1 || !owner.Finished() || client.publications.Load() != 1 || time.Since(startedAt) > 80*time.Second {
			t.Fatalf("expiry=%v renewals=%d attempts=%d starts=%d elapsed=%v", expired.Load(), renewals.Load(), attempts.Load(), backend.authored.Load(), time.Since(startedAt))
		}
	})
}

type preparationReplayConn struct {
	net.Conn
	reader io.Reader
}

func (c *preparationReplayConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

func TestPreparationAllocationAdmissionPrecedesPhysicalEffects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := preparationOwnerFixture(t)
		denied := errors.New("host health rejects startup")
		o.admitStart = func(context.Context) error { return denied }
		o.stageImage = func(context.Context, *preparationStage, workerapi.AllocationIdentity, workerapi.PreparationStart) error {
			t.Fatal("staged before admission")
			return nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
		defer cancel()
		if err := o.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("startup error = %v", err)
		}
		if backend.starts.Load() != 0 || o.stage != nil || o.closing || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
			t.Fatal("denied startup acquired physical custody")
		}
		if _, err := NewPreparationAllocationOwner(o.machines, client, o.identity, o.hostEpoch, nil); err == nil {
			t.Fatal("missing admission accepted")
		}
	})
}

func TestPreparationAllocationHealthWaitRenewsSameGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, client, backend := preparationOwnerFixture(t)
		readyAt := time.Now().Add(75 * time.Second)
		o.admitStart = func(context.Context) error {
			if time.Now().Before(readyAt) {
				return errors.New("temporarily unhealthy")
			}
			return nil
		}
		if err := o.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if time.Now().Before(readyAt) || backend.starts.Load() != 1 || client.deliveries.Load() != 1 || client.renewals.Load() < 3 || !o.Finished() {
			t.Fatal("preparation did not renew and finish same grant after health recovered")
		}
	})
}

func TestPreparationAllocationKeyFaultReleasesUnstartedOwner(t *testing.T) {
	o, _, backend := preparationOwnerFixture(t)
	o.admitStart = func(context.Context) error { return ErrCheckpointKeyUnavailable }
	if err := o.Run(t.Context()); !errors.Is(err, ErrCheckpointKeyUnavailable) {
		t.Fatalf("fault: %v", err)
	}
	if !o.Finished() || backend.starts.Load() != 0 || len(o.machines.Reservations.Snapshot().Reservations) != 0 {
		t.Fatal("unstarted preparation remained held or started")
	}
}
