package executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

type actorTurnCommitControlPlane struct {
	*testRunLeaseControlPlane
	request   workerapi.CommitActorTurnRequest
	mismatch  bool
	commitErr error
	started   chan struct{}
	release   chan struct{}
}

func (c *actorTurnCommitControlPlane) CommitActorTurn(ctx context.Context, request workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error) {
	c.request = request
	if c.commitErr != nil {
		return workerapi.CommitActorTurnResponse{}, c.commitErr
	}
	if c.started != nil {
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
			return workerapi.CommitActorTurnResponse{}, ctx.Err()
		}
	}
	response := workerapi.CommitActorTurnResponse{EventID: "settlement-event", Lease: request.Lease, CorrelationID: request.CorrelationID, CommittedInputSequence: request.TargetInputSequence}
	if c.mismatch {
		response.CommittedInputSequence++
	}
	return response, nil
}

func TestTurnSettlementNeedsNoCaptureOrFrontierChange(t *testing.T) {
	for _, disposition := range []string{"completed", "failed"} {
		t.Run(disposition, func(t *testing.T) {
			claim := testFreshProgramClaim(t)
			target, err := workspace.EmptyResetTarget("version-1", workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
			if err != nil {
				t.Fatal(err)
			}
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
			cp := &actorTurnCommitControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{trace: &runLeaseTrace{}}}
			task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, execution: testTurnExecution(claim.Lease).Session}, controlPlane: cp, lease: claim.Lease, resetTarget: target}
			requested := &programv0.TurnSettleRequested{Execution: testTurnExecution(claim.Lease), CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1, Disposition: disposition}
			if disposition == "completed" {
				requested.ResultJson = new(`{"answer":42}`)
			} else {
				requested.ErrorJson = new(`{"code":"user_error"}`)
			}
			done := make(chan error, 1)
			go func() { done <- task.handleTurnSettle(t.Context(), requested) }()
			header, size, err := wire.ReadStreamFrameHeader(guest)
			if err != nil {
				t.Fatal(err)
			}
			// The first and only reply is the committed decision, not a freeze/capture request.
			decision, err := wire.ReadResumeDecision(header, guest, size)
			if err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			if decision.Kind != "committed" || decision.CorrelationId != requested.CorrelationId {
				t.Fatalf("decision=%+v", decision)
			}
			var payload map[string]any
			if err = json.Unmarshal([]byte(decision.DataJson), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 0 {
				t.Fatalf("payload=%v", payload)
			}
			if task.resetTarget != target || task.lease != claim.Lease || cp.request.Disposition != disposition {
				t.Fatal("settlement changed physical authority")
			}
		})
	}
}

type turnReleaseSession struct {
	fakeGuestSession
	releases   int
	releaseErr error
}

func (s *turnReleaseSession) ReleaseCheckpointSource(context.Context) error {
	s.releases++
	_ = s.stream.Close()
	return s.releaseErr
}

func TestTurnSettlementFailureStopsComputer(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit error", true: "mismatched receipt"}[mismatch], func(t *testing.T) {
			claim := testFreshProgramClaim(t)
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			commitErr, releaseErr := &httpclient.Error{StatusCode: http.StatusConflict}, errors.New("physical stop failed")
			cp := &actorTurnCommitControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{trace: &runLeaseTrace{}}, mismatch: mismatch}
			if !mismatch {
				cp.commitErr = commitErr
			}
			session := &turnReleaseSession{fakeGuestSession: fakeGuestSession{stream: host}, releaseErr: releaseErr}
			task := &guestRunLeaseTask{program: freshProgram{session: session, execution: testTurnExecution(claim.Lease).Session}, controlPlane: cp, lease: claim.Lease}
			err := task.handleTurnSettle(t.Context(), &programv0.TurnSettleRequested{Execution: testTurnExecution(claim.Lease), CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1, Disposition: "completed"})
			var stopErr *checkpointSourceReleaseError
			if !errors.As(err, &stopErr) || !errors.Is(err, releaseErr) || session.releases != 1 {
				t.Fatalf("err=%v releases=%d", err, session.releases)
			}
			if !mismatch && !errors.Is(err, commitErr) {
				t.Fatalf("lost commit cause: %v", err)
			}
		})
	}
}

type turnBlockedWrite struct {
	io.ReadWriteCloser
	started chan struct{}
}

func (s *turnBlockedWrite) Write(p []byte) (int, error) {
	close(s.started)
	return s.ReadWriteCloser.Write(p)
}

func TestTurnSettlementCancellationUnblocksDecisionAndStopsComputer(t *testing.T) {
	claim := testFreshProgramClaim(t)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	stream := &turnBlockedWrite{ReadWriteCloser: host, started: make(chan struct{})}
	session := &turnReleaseSession{fakeGuestSession: fakeGuestSession{stream: stream}}
	cp := &actorTurnCommitControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{trace: &runLeaseTrace{}}}
	task := &guestRunLeaseTask{program: freshProgram{session: session, execution: testTurnExecution(claim.Lease).Session}, controlPlane: cp, lease: claim.Lease}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- task.handleTurnSettle(ctx, &programv0.TurnSettleRequested{Execution: testTurnExecution(claim.Lease), CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1, Disposition: "completed"})
	}()
	select {
	case <-stream.started:
	case <-time.After(5 * time.Second):
		t.Fatal("decision write never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || session.releases != 1 {
			t.Fatalf("err=%v releases=%d", err, session.releases)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled decision remained blocked")
	}
}

type turnRenewalMounts struct{ WorkspaceMountSessionRegistry }

func (turnRenewalMounts) RenewWorkspaceAuthority(_ context.Context, request *workspacev0.RenewWorkspaceAuthorityRequest) (*workspacev0.WorkspaceAuthorityFence, error) {
	fence := proto.Clone(request.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
	fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	return fence, nil
}
func TestTurnSettlementAllowsConcurrentLeaseRenewal(t *testing.T) {
	claim := testFreshProgramClaim(t)
	next := claim.Lease
	next.ExpiresAt = next.ExpiresAt.Add(time.Minute)
	cp := &actorTurnCommitControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{trace: &runLeaseTrace{}, renewed: testRunLeaseRenewResponse(next)}, started: make(chan struct{}), release: make(chan struct{})}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	task := &guestRunLeaseTask{program: freshProgram{session: fakeGuestSession{stream: host}, execution: testTurnExecution(claim.Lease).Session}, controlPlane: cp, lease: claim.Lease, authority: freshWorkspaceAuthority(&claim, "channel"), mounts: turnRenewalMounts{}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- task.handleTurnSettle(ctx, &programv0.TurnSettleRequested{Execution: testTurnExecution(claim.Lease), CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1, Disposition: "completed"})
	}()
	select {
	case <-cp.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := task.RenewRunLease(ctx); err != nil {
		t.Fatal(err)
	}
	close(cp.release)
	header, size, err := wire.ReadStreamFrameHeader(guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wire.ReadResumeDecision(header, guest, size); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if task.lease.ExpiresAt != next.ExpiresAt || task.lease.BaseWorkspaceVersionID != claim.Lease.BaseWorkspaceVersionID {
		t.Fatal("renewal changed settlement base or failed to advance expiry")
	}
}
