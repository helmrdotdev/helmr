package computerhost

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type checkpointReconcileClient struct {
	onAbort         func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error)
	onAbortComplete func(context.Context, workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error)
	onTargets       func(context.Context) (workerapi.InstanceReconcileResponse, error)

	target                    workerapi.InstanceReconcileTarget
	registered, ready, closed int
	registerError, readyError error
	onReady                   func()
	onRegister                func(context.Context) error
	instanceFailures          []workerapi.ComputerInstanceStateRequest
	closedProof               string
}

func (c *checkpointReconcileClient) AbortCapture(ctx context.Context, q workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
	if c.onAbort != nil {
		return c.onAbort(ctx, q)
	}
	return workerapi.CaptureAbortResponse{}, errors.New("unexpected capture abort")
}
func (c *checkpointReconcileClient) CompleteCaptureAbort(ctx context.Context, q workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error) {
	if c.onAbortComplete != nil {
		return c.onAbortComplete(ctx, q)
	}
	return workerapi.ComputerCheckpointResponse{}, errors.New("unexpected capture abort acknowledgment")
}
func (c *checkpointReconcileClient) ListInstanceReconcileTargets(ctx context.Context) (workerapi.InstanceReconcileResponse, error) {
	if c.onTargets != nil {
		return c.onTargets(ctx)
	}
	return workerapi.InstanceReconcileResponse{}, errors.New("unexpected capture status read")
}

func (c *checkpointReconcileClient) receipt() workerapi.ComputerCheckpointResponse {
	return workerapi.ComputerCheckpointResponse{ComputerInstanceID: c.target.ID, WorkerEpoch: c.target.WorkerEpoch, DesiredVersion: c.target.DesiredVersion, CheckpointID: c.target.Capture.CheckpointID, ComputerDiskVersionID: "saved-version"}
}
func (c *checkpointReconcileClient) RegisterCheckpoint(ctx context.Context, _ workerapi.RegisterCheckpointRequest) (workerapi.ComputerCheckpointResponse, error) {
	c.registered++
	if c.onRegister != nil {
		return c.receipt(), c.onRegister(ctx)
	}
	return c.receipt(), c.registerError
}
func (c *checkpointReconcileClient) MarkCheckpointReady(context.Context, workerapi.CheckpointReadyRequest) (workerapi.ComputerCheckpointResponse, error) {
	c.ready++
	if c.onReady != nil {
		c.onReady()
	}
	return c.receipt(), c.readyError
}

func (c *checkpointReconcileClient) RegisterCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error {
	return nil
}
func (c *checkpointReconcileClient) CertifyCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error {
	return nil
}
func (c *checkpointReconcileClient) ReuseCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error {
	return nil
}
func (c *checkpointReconcileClient) MarkComputerInstanceReady(context.Context, workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	return workerapi.ComputerInstance{}, errors.New("unexpected ready")
}
func (c *checkpointReconcileClient) MarkComputerInstanceFailed(_ context.Context, request workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	c.instanceFailures = append(c.instanceFailures, request)
	return workerapi.ComputerInstance{}, &httpclient.Error{StatusCode: 409, Message: "stale desired version"}
}
func (c *checkpointReconcileClient) MarkComputerInstanceClosed(_ context.Context, r workerapi.ComputerInstanceStateRequest) (workerapi.ComputerInstance, error) {
	if r.DesiredVersion != c.target.DesiredVersion+1 || r.CleanupProof == nil {
		return workerapi.ComputerInstance{}, errors.New("missing closure fence/proof")
	}
	c.closed++
	c.closedProof = r.CleanupProof.Method
	return workerapi.ComputerInstance{}, nil
}

// allowCaptureAbort supplies the two guest acknowledgments and the matching
// durable Control Plane receipt for an idle source.
func allowCaptureAbort(t *testing.T, client *checkpointReconcileClient, machine *checkpointMachine) {
	t.Helper()
	target := client.target
	installed := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1})
	activated := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1, Activated: true})
	machine.streams = []io.ReadWriteCloser{machine.stream, installed, activated}
	client.onAbort = func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
		return abortReceipt(target), nil
	}
	client.onAbortComplete = func(context.Context, workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error) {
		receipt := client.receipt()
		receipt.DesiredVersion++
		return receipt, nil
	}
}

// authorizeCaptureClose models a subsequent durable close intent, which may
// exclude the source even when the preceding capture did not complete.
func authorizeCaptureClose(client *checkpointReconcileClient) {
	client.onTargets = func(context.Context) (workerapi.InstanceReconcileResponse, error) {
		target := client.target
		target.DesiredVersion++
		target.Action = workerapi.InstanceReconcileClose
		return workerapi.InstanceReconcileResponse{Items: []workerapi.InstanceReconcileTarget{target}}, nil
	}
}

func TestPreparedMachinesCheckpointIdleSource(t *testing.T) {
	for _, failure := range []string{"", "register", "ready"} {
		t.Run(failure, func(t *testing.T) {
			target := checkpointCaptureTarget(0)
			machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
			client := &checkpointReconcileClient{target: target}
			if failure != "" {
				allowCaptureAbort(t, client, machine)
			}
			if failure == "register" {
				client.registerError = &httpclient.Error{StatusCode: 409, Message: "registration rejected"}
			}
			if failure == "ready" {
				client.readyError = &httpclient.Error{StatusCode: 409, Message: "readiness rejected"}
			}
			ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
			p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, machine)}
			client.onReady = func() {
				if machine.closeCount != 0 {
					t.Fatal("source excluded before ready receipt")
				}
			}
			err := p.captureInstanceTarget(t.Context(), client, target)
			if (err != nil) != (failure != "") {
				t.Fatalf("capture=%v", err)
			}
			if client.registered != 1 {
				t.Fatalf("registration=%d", client.registered)
			}
			if failure == "" && (client.closed != 1 || machine.closeCount != 1 || p.instanceCheckedOut(target.ID, target.WorkerEpoch)) {
				t.Fatal("successful capture did not exclude source")
			}
			if failure != "" && (client.closed != 0 || machine.closeCount != 0 || machine.resumeCount != 1 || captureRetained(p, ref)) {
				t.Fatal("failed capture did not resume same source")
			}
			if failure == "" && client.ready != 1 {
				t.Fatalf("ready=%d", client.ready)
			}
		})
	}
}

func TestCheckpointComputerPublisherKeepsSourceFence(t *testing.T) {
	// Publication uses the same operation identity for all phases, with no Run.
	client := &checkpointPublicationRecorder{}
	request := workerapi.CheckpointComputerObjectRequest{ComputerInstanceID: "instance", WorkerEpoch: 3, DesiredVersion: 4, CheckpointID: "checkpoint"}
	p := checkpointComputerPublisher{client: client, request: request}
	e := blockformat.ObjectInspection{Pack: &blockformat.PackInspection{}}
	if err := p.Register(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if err := p.Certify(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if err := p.Reuse(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 3 {
		t.Fatal("missing phases")
	}
	for _, r := range client.requests {
		if r.ComputerInstanceID != request.ComputerInstanceID || r.WorkerEpoch != 3 || r.DesiredVersion != 4 || r.CheckpointID != request.CheckpointID || r.Inspection.Pack == nil {
			t.Fatalf("changed source=%+v", r)
		}
	}
}

type checkpointPublicationRecorder struct {
	requests []workerapi.CheckpointComputerObjectRequest
}

func (c *checkpointPublicationRecorder) RegisterCheckpointComputerObject(_ context.Context, r workerapi.CheckpointComputerObjectRequest) error {
	c.requests = append(c.requests, r)
	return nil
}
func (c *checkpointPublicationRecorder) CertifyCheckpointComputerObject(_ context.Context, r workerapi.CheckpointComputerObjectRequest) error {
	c.requests = append(c.requests, r)
	return nil
}
func (c *checkpointPublicationRecorder) ReuseCheckpointComputerObject(_ context.Context, r workerapi.CheckpointComputerObjectRequest) error {
	c.requests = append(c.requests, r)
	return nil
}

func TestPreparedMachinesCheckpointRetriesExclusionAndStagingCleanup(t *testing.T) {
	for _, failedCapture := range []bool{false, true} {
		name := "ready"
		if failedCapture {
			name = "registration failed"
		}
		t.Run(name, func(t *testing.T) {
			target := checkpointCaptureTarget(0)
			artifact := checkpointArtifact(t)
			machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: artifact, closeErr: errors.New("stop temporarily unavailable")}
			client := &checkpointReconcileClient{target: target}
			if failedCapture {
				authorizeCaptureClose(client)
				client.registerError = &httpclient.Error{StatusCode: 409, Message: "registration rejected"}
			}
			ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
			p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, machine)}
			if err := p.captureInstanceTarget(t.Context(), client, target); err == nil {
				t.Fatal("failed exclusion reported success")
			}
			if !p.instanceCheckedOut(target.ID, target.WorkerEpoch) || !captureRetained(p, ref) || client.closed != 0 {
				t.Fatal("failed exclusion lost its owner")
			}
			if failedCapture && p.Reservations.Snapshot().Used.HostDiskBytes == 0 {
				t.Fatal("uncertain source lost staging charge")
			}
			machine.closeErr = nil
			closeTarget := target
			closeTarget.DesiredVersion++
			closeTarget.Action = workerapi.InstanceReconcileClose
			if err := p.stopInstanceTarget(t.Context(), client, closeTarget); err != nil {
				t.Fatal(err)
			}
			if p.instanceCheckedOut(target.ID, target.WorkerEpoch) || captureRetained(p, ref) || client.closed != 1 || p.Reservations.Snapshot().Used.HostDiskBytes != 0 {
				t.Fatalf("cleanup incomplete closed=%d capacity=%+v", client.closed, p.Reservations.Snapshot().Used)
			}
			assertRemoved(t, artifact.VMState.Path)
		})
	}
}

func TestPreparedMachinesCheckpointJoinsMembersAndPauseFailure(t *testing.T) {
	for _, failPause := range []bool{false, true} {
		name := "shared"
		if failPause {
			name = "pause failure"
		}
		t.Run(name, func(t *testing.T) {
			target := checkpointCaptureTarget(2)
			machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
			client := &checkpointReconcileClient{target: target}
			ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
			registry := &CaptureRuns{}
			if failPause {
				authorizeCaptureClose(client)
			}
			p := &PreparedMachines{ComputerCaptures: registry, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, machine)}
			done := make(chan error, 2)
			for index, member := range target.Capture.Runs {
				wait := captureRegistryWait(t, registry, target, member)
				go func() {
					pause := <-wait.requests
					var err error
					if failPause && index == 0 {
						err = errors.New("pause failed")
					}
					pause.ready <- err
					<-pause.finished
					done <- pause.result
				}()
			}
			admitted := false
			err := p.reconcileInstanceTarget(t.Context(), client, target, func() { admitted = true })
			if !admitted || (err != nil) != failPause {
				t.Fatalf("admitted=%v err=%v", admitted, err)
			}
			for range 2 {
				result := <-done
				if (result != nil) != failPause {
					t.Fatalf("member result=%v", result)
				}
			}
			if client.closed != 1 {
				t.Fatal("members released without physical closure proof")
			}
			if failPause && (client.registered != 0 || len(machine.snapshotRequests) != 0) {
				t.Fatalf("failed pause captured: %+v", client)
			}
		})
	}
}

func TestPreparedMachinesCheckpointKeepsPollingDuringCapture(t *testing.T) {
	target := checkpointCaptureTarget(0)
	machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
	registered := make(chan struct{})
	checkpoints := &checkpointReconcileClient{target: target, onRegister: func(ctx context.Context) error {
		close(registered)
		<-ctx.Done()
		return ctx.Err()
	}}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	p := &PreparedMachines{Size: 2, ComputerCaptures: &CaptureRuns{}, Checkpoints: checkpoints, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, machine)}
	client := &batchInstanceClient{response: workerapi.InstanceReconcileResponse{Items: []workerapi.InstanceReconcileTarget{target}}, polled: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.ReconcileDesiredInstances(ctx, client) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("reconcile=%v", err)
		}
	}()
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not register")
	}
	for range 2 {
		select {
		case <-client.polled:
		case <-time.After(3 * time.Second):
			t.Fatal("capture prevented next desired-state poll")
		}
	}
}

func TestPreparedMachinesCheckpointRecoversUncertainReadyReceipt(t *testing.T) {
	target := checkpointCaptureTarget(0)
	machine := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
	// A definitive abort query discovers that the earlier ready request committed.
	client := &checkpointReconcileClient{target: target, readyError: &httpclient.Error{StatusCode: 409, Message: "candidate no longer pending"}}
	client.onAbort = func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
		if machine.closeCount != 0 {
			t.Error("source closed before adoption was known")
		}
		receipt := abortReceipt(target)
		receipt.Disposition = workerapi.CaptureAdopted
		return receipt, nil
	}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, machine)}
	if err := p.captureInstanceTarget(t.Context(), client, target); err == nil {
		t.Fatal("lost readiness error")
	}
	if len(client.instanceFailures) != 0 || client.closed != 1 || machine.closeCount != 1 || machine.resumeCount != 0 || captureRetained(p, ref) || p.instanceCheckedOut(target.ID, target.WorkerEpoch) {
		t.Fatalf("adopted capture did not exclude exactly once: closed=%d physical=%d resumed=%d failures=%v", client.closed, machine.closeCount, machine.resumeCount, client.instanceFailures)
	}
}

// unmountedCaptureClaim is capture's claim on a prepared machine that no
// Server has mounted.
func unmountedCaptureClaim(ref preparedMachineRef, target workerapi.InstanceReconcileTarget, machine vm.CheckpointableMachine) map[preparedMachineRef]*machineClaim {
	return map[preparedMachineRef]*machineClaim{ref: {gen: 1, kind: captureClaim, entry: preparedMachineEntry{target: target, machine: machine}}}
}

func captureRetained(p *PreparedMachines, ref preparedMachineRef) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	claim := p.claims[ref]
	return claim != nil && claim.checkpointer != nil
}

func TestPreparedMachinesCheckpointReleasesSource(t *testing.T) {
	t.Run("served through its mount", func(t *testing.T) {
		target := checkpointCaptureTarget(0)
		raw := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
		client := &checkpointReconcileClient{target: target}
		ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
		mount := newInstanceMount(raw)
		p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir()}
		p.mu.Lock()
		claim := p.claimLocked(ref, serverClaim, preparedMachineEntry{target: target, machine: raw})
		claim.mount = mount
		p.mu.Unlock()
		server := &machineCheckout{machines: p, ref: ref, gen: claim.gen, machine: raw, mount: mount}
		if err := p.captureInstanceTarget(t.Context(), client, target); err != nil {
			t.Fatal(err)
		}
		if released, err := mount.CheckpointReleaseResult(t.Context()); !released || err != nil {
			t.Fatalf("mount release=%v err=%v", released, err)
		}
		mount.saves.mu.Lock()
		quiesced := mount.saves.stopped
		mount.saves.mu.Unlock()
		if raw.closeCount != 1 || !quiesced {
			t.Fatalf("machine closes=%d saves quiesced=%v", raw.closeCount, quiesced)
		}
		if server.beginTeardown() {
			t.Fatal("server handle still holds the claim capture took over")
		}
		if client.ready != 1 || client.closed != 1 {
			t.Fatalf("ready=%d closed=%d", client.ready, client.closed)
		}
	})
	t.Run("unmounted directly", func(t *testing.T) {
		target := checkpointCaptureTarget(0)
		raw := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
		client := &checkpointReconcileClient{target: target}
		ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
		p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, raw)}
		if err := p.captureInstanceTarget(t.Context(), client, target); err != nil {
			t.Fatal(err)
		}
		if raw.closeCount != 1 || client.ready != 1 || client.closed != 1 {
			t.Fatalf("machine closes=%d ready=%d closed=%d", raw.closeCount, client.ready, client.closed)
		}
	})
}
