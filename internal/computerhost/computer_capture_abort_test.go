package computerhost

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func abortReceipt(target workerapi.InstanceReconcileTarget) workerapi.CaptureAbortResponse {
	return workerapi.CaptureAbortResponse{WorkerHostID: "worker", ComputerInstanceID: target.ID, ComputerID: target.Source.ComputerID, WorkerEpoch: target.WorkerEpoch, DesiredVersion: target.DesiredVersion, AbortDesiredVersion: target.DesiredVersion + 1, CheckpointID: target.Capture.CheckpointID, WriterGeneration: target.Source.WriterGeneration, MembershipRevision: target.Capture.MembershipRevision, VMPlatformID: target.Source.VMPlatformID, Disposition: workerapi.CaptureAborted}
}

func TestCaptureAbortRetainsOriginalMachineAndCheckout(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "known abort", true: "lost acknowledgment"}[lostReply], func(t *testing.T) {
			target := checkpointCaptureTarget(0)
			freeze := checkpointFreezeStream(t, target)
			installed := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1})
			activated := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1, Activated: true})
			session := &checkpointMachine{stream: freeze, streams: []io.ReadWriteCloser{freeze, installed, activated}, artifact: checkpointArtifact(t)}
			ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
			client := &checkpointReconcileClient{target: target, registerError: &httpclient.Error{StatusCode: 409, Message: "candidate rejected"}}
			p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, session)}
			claim := p.claims[ref]
			claim.kind = serverClaim
			initialGen := claim.gen
			completed := false
			client.onAbort = func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
				if claim.gen != initialGen || session.closeCount != 0 || claim.checkpointer == nil {
					t.Error("original owner was released before decision")
				}
				receipt := abortReceipt(target)
				if completed {
					receipt.Disposition = workerapi.CaptureAbortAcknowledged
				}
				return receipt, nil
			}
			client.onAbortComplete = func(context.Context, workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error) {
				if session.resumeCount != 1 || activated.written.Len() == 0 || p.Reservations.Snapshot().Used.GuestEphemeralDiskBytes != 0 {
					t.Error("acknowledgment preceded source resume/staging join")
				}
				completed = true
				receipt := client.receipt()
				receipt.DesiredVersion++
				if lostReply {
					return workerapi.ComputerCheckpointResponse{}, errors.New("acknowledgment response lost")
				}
				return receipt, nil
			}
			client.onTargets = func(context.Context) (workerapi.InstanceReconcileResponse, error) {
				return workerapi.InstanceReconcileResponse{}, errors.New("temporary control outage")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := p.captureInstanceTarget(ctx, client, target)
			if err == nil || !completed || session.closeCount != 0 || session.resumeCount != 1 || client.closed != 0 {
				t.Fatalf("capture=%v complete=%v closed=%d resumed=%d", err, completed, session.closeCount, session.resumeCount)
			}
			if p.claims[ref] != claim || claim.gen != initialGen || claim.kind != serverClaim || claim.checkpointer != nil {
				t.Fatal("abort did not restore original checkout")
			}
		})
	}
}

func TestCaptureAbortUnknownOutcomeRetainsSource(t *testing.T) {
	target := checkpointCaptureTarget(0)
	freeze := checkpointFreezeStream(t, target)
	installed := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1})
	activated := newCheckpointStream(t, nil, &computerv0.ComputerCaptureAbortResponse{CheckpointId: target.Capture.CheckpointID, AbortDesiredVersion: target.DesiredVersion + 1, Activated: true})
	session := &checkpointMachine{stream: freeze, streams: []io.ReadWriteCloser{freeze, installed, activated}, artifact: checkpointArtifact(t)}
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	client := &checkpointReconcileClient{target: target, registerError: &httpclient.Error{StatusCode: 409, Message: "candidate rejected"}}
	p := &PreparedMachines{ComputerCaptures: &CaptureRuns{}, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, session)}
	uncertain := make(chan struct{})
	recovered := make(chan struct{})
	attempts := 0
	client.onAbort = func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
		attempts++
		if attempts == 1 {
			return workerapi.CaptureAbortResponse{}, errors.New("abort response lost")
		}
		return abortReceipt(target), nil
	}
	client.onTargets = func(ctx context.Context) (workerapi.InstanceReconcileResponse, error) {
		close(uncertain)
		select {
		case <-recovered:
		case <-ctx.Done():
			return workerapi.InstanceReconcileResponse{}, ctx.Err()
		}
		return workerapi.InstanceReconcileResponse{}, errors.New("status unavailable")
	}
	client.onAbortComplete = func(context.Context, workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error) {
		receipt := client.receipt()
		receipt.DesiredVersion++
		return receipt, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.captureInstanceTarget(ctx, client, target) }()
	select {
	case <-uncertain:
	case <-ctx.Done():
		t.Fatal("abort did not reach uncertain result")
	}
	if session.closeCount != 0 || session.resumeCount != 0 || !captureRetained(p, ref) || p.Reservations.Snapshot().Used.GuestEphemeralDiskBytes == 0 {
		t.Error("unknown outcome changed source ownership")
	}
	select {
	case err := <-done:
		t.Errorf("capture owner left unknown source: %v", err)
	default:
	}
	close(recovered)
	if err := <-done; err == nil {
		t.Fatal("lost original capture error")
	}
	if session.closeCount != 0 || session.resumeCount != 1 || captureRetained(p, ref) {
		t.Fatal("recovered abort did not retain same machine")
	}
}

func TestCaptureAbortRestoresHealthyMemberAndKeepsCancellation(t *testing.T) {
	for _, partialPause := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication failure", true: "partial pause"}[partialPause], func(t *testing.T) {
			target := checkpointCaptureTarget(2)
			session := &checkpointMachine{stream: checkpointFreezeStream(t, target), artifact: checkpointArtifact(t)}
			client := &checkpointReconcileClient{target: target, registerError: &httpclient.Error{StatusCode: 409, Message: "candidate rejected"}}
			allowCaptureAbort(t, client, session)
			if partialPause {
				// No whole-Computer freeze was attempted after member pause failed.
				session.streams = session.streams[1:]
			}
			registry := &CaptureRuns{}
			ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
			p := &PreparedMachines{ComputerCaptures: registry, Checkpoints: client, CheckpointEncryptor: testCheckpointEncryptor(t), ComputerObjects: &captureStore{}, Reservations: testCheckpointReservations(t), TempDir: t.TempDir(), claims: unmountedCaptureClaim(ref, target, session)}
			receipt := abortReceipt(target)
			receipt.WriteCapability = "same-source-capability"
			type outcome struct {
				err     error
				resumed bool
			}
			results := []chan outcome{make(chan outcome, 1), make(chan outcome, 1)}
			var phases []bool
			waits := make([]*CaptureWait, 0, 2)
			for index, member := range target.Capture.Runs {
				wait := captureRegistryWait(t, registry, target, member)
				waits = append(waits, wait)
				receipt.Members = append(receipt.Members, workerapi.CaptureAbortMember{RunID: member.RunID, AttemptNumber: member.AttemptNumber, RunWaitID: member.RunWaitID, Lease: workerapi.RunLeaseFence{ID: member.RunLeaseID, LeaseSequence: wait.lease.LeaseSequence}, ExpiresAt: wait.lease.ExpiresAt, Cancelled: index == 1})
				wait.resume = func(_ context.Context, _ workerapi.InstanceReconcileTarget, grant workerapi.CaptureAbortMember, restore bool) (*computerv0.ComputerRunAuthority, uint64, error) {
					if index != 0 || grant.Cancelled {
						t.Error("cancelled member received renewal authority")
					}
					phases = append(phases, restore)
					return &computerv0.ComputerRunAuthority{WriteCapability: receipt.WriteCapability, Fence: &computerv0.ComputerAuthorityFence{WorkerHostId: receipt.WorkerHostID}}, 0, nil
				}
				go func() {
					pause := <-wait.Pauses()
					var err error
					if partialPause && index == 0 {
						err = errors.New("member pause interrupted")
					}
					err = pause.Settle(err)
					results[index] <- outcome{err, pause.Resumed()}
				}()
			}
			client.onAbort = func(context.Context, workerapi.CaptureAbortRequest) (workerapi.CaptureAbortResponse, error) {
				return receipt, nil
			}
			client.onAbortComplete = func(_ context.Context, q workerapi.CaptureAbortCompleteRequest) (workerapi.ComputerCheckpointResponse, error) {
				if len(q.CancelledRunLeaseIDs) != 1 || q.CancelledRunLeaseIDs[0] != target.Capture.Runs[1].RunLeaseID {
					t.Error("acknowledgment lost cancelled disposition")
				}
				for _, result := range results {
					select {
					case <-result:
						t.Error("member returned before durable acknowledgment")
					default:
					}
				}
				ack := client.receipt()
				ack.DesiredVersion++
				return ack, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := p.captureInstanceTarget(ctx, client, target); err == nil {
				t.Fatal("original capture failure was lost")
			}
			for index, result := range results {
				r := <-result
				if index == 0 && (!r.resumed || (r.err != nil) != partialPause) {
					t.Fatalf("source-resumed member lost its own pause outcome: %+v", r)
				}
				if index == 1 && (r.err == nil || r.resumed) {
					t.Fatalf("cancelled member revived=%+v", r)
				}
				detachErr := waits[index].Detach()
				if index == 0 && detachErr != nil {
					t.Fatalf("resumed member inherited capture error on detach: %v", detachErr)
				}
			}
			if len(phases) != 2 || phases[0] || !phases[1] || session.closeCount != 0 || captureRetained(p, ref) {
				t.Fatalf("abort phases=%v closes=%d", phases, session.closeCount)
			}
			wait, err := registry.Register(waits[0].lease, waits[0].waitID, waits[0].resume)
			if err != nil {
				t.Fatalf("healthy original wait cannot continue: %v", err)
			}
			_ = wait.Detach()
		})
	}
}
