package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type checkpointReconcileClient struct {
	target                            workerapi.RuntimeReconcileTarget
	registered, ready, failed, closed int
	registerError, readyError         error
	onReady                           func()
	onRegister                        func(context.Context) error
	failedError                       error
	instanceFailures                  []workerapi.ComputerInstanceStateRequest
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
func (c *checkpointReconcileClient) MarkCheckpointFailed(context.Context, workerapi.CheckpointFailedRequest) (workerapi.ComputerCheckpointResponse, error) {
	c.failed++
	return c.receipt(), c.failedError
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
	return workerapi.ComputerInstance{}, nil
}

func TestComputerCheckpointPoolIdleSource(t *testing.T) {
	for _, failure := range []string{"", "register", "ready"} {
		t.Run(failure, func(t *testing.T) {
			target := checkpointCaptureTarget(0)
			session := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
			client := &checkpointReconcileClient{target: target}
			if failure == "register" {
				client.registerError = &httpclient.Error{StatusCode: 409, Message: "registration rejected"}
			}
			if failure == "ready" {
				client.readyError = &httpclient.Error{StatusCode: 409, Message: "readiness rejected"}
			}
			ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
			p := &PreparedRuntimePool{ComputerCaptures: &ComputerCaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
			client.onReady = func() {
				if session.closeCount != 0 {
					t.Fatal("source excluded before ready receipt")
				}
			}
			err := p.captureRuntimeTarget(t.Context(), client, target)
			if (err != nil) != (failure != "") {
				t.Fatalf("capture=%v", err)
			}
			if client.registered != 1 || client.closed != 1 || session.closeCount < 1 || p.runtimeCheckedOut(target.ID, target.WorkerEpoch) {
				t.Fatalf("registration=%d closure=%d source closes=%d", client.registered, client.closed, session.closeCount)
			}
			if failure == "" && (client.ready != 1 || client.failed != 0) {
				t.Fatalf("ready=%d failed=%d", client.ready, client.failed)
			}
			if failure != "" && client.failed != 1 {
				t.Fatalf("failure receipts=%d", client.failed)
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

func TestComputerCheckpointPoolRetriesExclusionAndStagingCleanup(t *testing.T) {
	for _, failedCapture := range []bool{false, true} {
		name := "ready"
		if failedCapture {
			name = "registration failed"
		}
		t.Run(name, func(t *testing.T) {
			target := checkpointCaptureTarget(0)
			artifact := checkpointArtifact(t)
			session := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: artifact, closeErr: errors.New("stop temporarily unavailable")}
			client := &checkpointReconcileClient{target: target}
			if failedCapture {
				client.registerError = &httpclient.Error{StatusCode: 409, Message: "registration rejected"}
			}
			ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
			p := &PreparedRuntimePool{ComputerCaptures: &ComputerCaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
			if err := p.captureRuntimeTarget(t.Context(), client, target); err == nil {
				t.Fatal("failed exclusion reported success")
			}
			if !p.runtimeCheckedOut(target.ID, target.WorkerEpoch) || p.captureCleanup[ref] == nil || client.closed != 0 {
				t.Fatal("failed exclusion lost its owner")
			}
			if failedCapture && p.Reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
				t.Fatal("uncertain source lost staging charge")
			}
			session.closeErr = nil
			closeTarget := target
			closeTarget.DesiredVersion++
			closeTarget.Action = workerapi.RuntimeReconcileClose
			if err := p.StopRuntimeTarget(t.Context(), client, closeTarget); err != nil {
				t.Fatal(err)
			}
			if p.runtimeCheckedOut(target.ID, target.WorkerEpoch) || p.captureCleanup[ref] != nil || client.closed != 1 || p.Reservations.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
				t.Fatalf("cleanup incomplete closed=%d capacity=%+v", client.closed, p.Reservations.Snapshot().Used)
			}
			assertRemoved(t, artifact.VMState.Path)
		})
	}
}

func TestComputerCheckpointPoolJoinsMembersAndPauseFailure(t *testing.T) {
	for _, failPause := range []bool{false, true} {
		name := "shared"
		if failPause {
			name = "pause failure"
		}
		t.Run(name, func(t *testing.T) {
			target := checkpointCaptureTarget(2)
			session := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
			client := &checkpointReconcileClient{target: target}
			ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
			registry := &ComputerCaptureRuns{}
			p := &PreparedRuntimePool{ComputerCaptures: registry, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
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
			err := p.reconcileRuntimeTarget(t.Context(), client, target, func() { admitted = true })
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
			if failPause && (client.failed != 1 || client.registered != 0 || len(session.snapshotRequests) != 0) {
				t.Fatalf("failed pause captured: %+v", client)
			}
		})
	}
}

func TestComputerCheckpointPoolKeepsPollingDuringCapture(t *testing.T) {
	target := checkpointCaptureTarget(0)
	session := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
	registered := make(chan struct{})
	checkpoints := &checkpointReconcileClient{target: target, onRegister: func(ctx context.Context) error {
		close(registered)
		<-ctx.Done()
		return ctx.Err()
	}}
	ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
	p := &PreparedRuntimePool{Size: 2, ComputerCaptures: &ComputerCaptureRuns{}, Checkpoints: checkpoints, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
	client := &batchRuntimeClient{response: workerapi.RuntimeReconcileResponse{Items: []workerapi.RuntimeReconcileTarget{target}}, polled: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.ReconcileDesiredRuntimes(ctx, client) }()
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

func TestComputerCheckpointPoolRecoversUncertainReadyReceipt(t *testing.T) {
	target := checkpointCaptureTarget(0)
	session := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The server committed ready, but the client lost the response and its deadline.
	client := &checkpointReconcileClient{target: target, onReady: cancel, readyError: context.Canceled, failedError: &httpclient.Error{StatusCode: 409, Message: "checkpoint already ready"}}
	connector := &countingRuntimeBackend{}
	ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
	p := &PreparedRuntimePool{Backend: connector, ComputerCaptures: &ComputerCaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
	if err := p.captureRuntimeTarget(ctx, client, target); err == nil {
		t.Fatal("lost receipt reported success")
	}
	if len(client.instanceFailures) != 1 || client.instanceFailures[0].DesiredVersion != target.DesiredVersion || client.instanceFailures[0].CleanupProof == nil {
		t.Fatalf("uncertain receipt changed authority: %+v", client.instanceFailures)
	}
	if session.closeCount == 0 || p.runtimeCheckedOut(target.ID, target.WorkerEpoch) || p.captureCleanup[ref] != nil || client.closed != 0 {
		t.Fatal("unknown commit must exclude source without fabricating a close version")
	}
	closeTarget := target
	closeTarget.DesiredVersion++
	closeTarget.Action = workerapi.RuntimeReconcileClose
	if err := p.StopRuntimeTarget(t.Context(), client, closeTarget); err != nil {
		t.Fatal(err)
	}
	if client.closed != 1 || connector.calls.Load() != 1 {
		t.Fatal("fresh desired state did not reconcile excluded source")
	}
}

type captureSourceWithMountOperations struct {
	*checkpointSession
	releases, quiesces int
}

func (s *captureSourceWithMountOperations) ReleaseCheckpointSource(context.Context) error {
	s.releases++
	return nil
}

func (s *captureSourceWithMountOperations) QuiesceComputerSaves(context.Context) error {
	s.quiesces++
	return nil
}

func TestComputerCheckpointPoolReleasesSourceThroughMachineClose(t *testing.T) {
	target := checkpointCaptureTarget(0)
	raw := &checkpointSession{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
	session := &captureSourceWithMountOperations{checkpointSession: raw}
	client := &checkpointReconcileClient{target: target}
	ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
	p := &PreparedRuntimePool{ComputerCaptures: &ComputerCaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), checkedOut: map[preparedRuntimeRef]struct{}{ref: {}}, checkedOutEntries: map[preparedRuntimeRef]preparedRuntimeEntry{ref: {target: target, session: session}}}
	if err := p.captureRuntimeTarget(t.Context(), client, target); err != nil {
		t.Fatal(err)
	}
	if raw.closeCount != 1 || session.releases != 0 || session.quiesces != 0 {
		t.Fatalf("machine closes=%d mount releases=%d save quiesces=%d", raw.closeCount, session.releases, session.quiesces)
	}
	if client.ready != 1 || client.closed != 1 {
		t.Fatalf("ready=%d closed=%d", client.ready, client.closed)
	}
}
