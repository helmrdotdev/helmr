package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func finalizingActorRequest(t *testing.T) (*actorCheckpointFixture, workerapi.CompleteActorRequest) {
	t.Helper()
	f := newActorCheckpointFixture(t)
	a := f.claim
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{run: a.run, attempt: a.attempt, runtime: a.runtime, runLease: a.runLease, workspace: a.workspace, workspaceMount: a.workspaceMount, workspaceLease: a.workspaceLease})
	if err != nil {
		t.Fatal(err)
	}
	var began workerapi.BeginRunFinalizationResponse
	f.workerCall(t, f.server.workerBeginRunFinalization, workerapi.BeginRunFinalizationRequest{Lease: f.fence(), ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: 1, RunLeaseID: f.fence().ID}, OperationID: uuid.NewV7().String(), Kind: workerapi.RunFinalizationCapture}, &began)
	assignment.ExpiresAt = began.ExpiresAt
	capture := validTaskWorkspaceCapture(t, assignment)
	capture.Receipt.OperationID = began.OperationID
	capture.Disk.LogicalBytes = a.runtime.ReservedGuestEphemeralDiskBytes
	setCaptureFingerprint(t, capture)
	return f, workerapi.CompleteActorRequest{Lease: f.fence(), Outcome: workerapi.ActorOutcome{RunGeneration: a.actor.RunGeneration, Failed: &workerapi.TaskFailure{Message: "failed after writes"}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
}

func TestRunFinalizationRegistrationPostgres(t *testing.T) {
	f, req := finalizingActorRequest(t)
	capture := req.Workspace.Captured
	registration := workerapi.RegisterRunFinalizationRequest{Lease: req.Lease, OperationID: capture.Receipt.OperationID, Disk: capture.Disk}
	// Register before any object exists remotely; exact replay is accepted.
	for i := 0; i < 2; i++ {
		if err := f.server.registerRunFinalization(t.Context(), f.worker, registration); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_finalization_objects WHERE run_lease_id=$1`, f.claim.runLease.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("registrations=%d %v", count, err)
	}
	for _, mode := range []string{"digest", "size", "operation", "computer", "capacity", "epoch"} {
		t.Run(mode, func(t *testing.T) {
			changed := registration
			switch mode {
			case "digest":
				changed.Disk.Artifact.Digest = "sha256:" + strings.Repeat("b", 64)
			case "size":
				changed.Disk.Artifact.SizeBytes++
			case "operation":
				changed.OperationID = uuid.NewV7().String()
			case "computer":
				changed.Disk.ComputerID = uuid.NewV7().String()
			case "capacity":
				changed.Disk.LogicalBytes += 4096
			case "epoch":
				changed.Lease.LeaseSequence++
			}
			if err := f.server.registerRunFinalization(t.Context(), f.worker, changed); err == nil {
				t.Fatal("changed candidate accepted")
			}
		})
	}
	// Direct retirement cannot bypass the live candidate pin.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE cas_object_lifetimes SET retired_at=clock_timestamp(),next_reclaim_at=clock_timestamp() WHERE digest=$1`, registration.Disk.Artifact.Digest); err == nil {
		t.Fatal("retired live registered disk")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='lost',terminal_at=clock_timestamp(),terminal_reason_code='worker_lost' WHERE id=$1`, f.claim.runLease.ID)
	q := db.New(f.Pool)
	if n, err := q.RetireAbandonedCasObject(t.Context(), registration.Disk.Artifact.Digest); err != nil || n != 1 {
		t.Fatalf("abandoned retirement=%d %v", n, err)
	}
	if err := f.server.registerRunFinalization(t.Context(), f.worker, registration); err == nil {
		t.Fatal("lost owner registered retired candidate")
	}
}

func TestRunFinalizationPublicationRequiresRegisteredDiskPostgres(t *testing.T) {
	f, req := finalizingActorRequest(t)
	obj, err := f.server.cas.Put(t.Context(), computer.DiskMediaType, strings.NewReader("opaque disk publication fixture"))
	if err != nil {
		t.Fatal(err)
	}
	req.Workspace.Captured.Disk.Artifact = workerapi.CheckpointArtifact{Digest: obj.Digest, SizeBytes: obj.SizeBytes, MediaType: obj.MediaType}
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if status := finalizationActorStatus(t, f, req); status != http.StatusConflict {
		t.Fatalf("unregistered completion status=%d", status)
	}
	var head uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id FROM computers WHERE id=$1`, f.workspaceID).Scan(&head); err != nil || head != f.rootID {
		t.Fatalf("unregistered publication changed head: %s %v", head, err)
	}
	registration := workerapi.RegisterRunFinalizationRequest{Lease: req.Lease, OperationID: req.Workspace.Captured.Receipt.OperationID, Disk: req.Workspace.Captured.Disk}
	if err := f.server.registerRunFinalization(t.Context(), f.worker, registration); err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.Workspace.Captured = cloneTaskWorkspaceCapture(req.Workspace.Captured)
	other, err := f.server.cas.Put(t.Context(), computer.DiskMediaType, strings.NewReader("different finalization disk"))
	if err != nil {
		t.Fatal(err)
	}
	changed.Workspace.Captured.Disk.Artifact = workerapi.CheckpointArtifact{Digest: other.Digest, SizeBytes: other.SizeBytes, MediaType: other.MediaType}
	if status := finalizationActorStatus(t, f, changed); status != http.StatusConflict {
		t.Fatalf("changed candidate status=%d", status)
	}
	for i := 0; i < 2; i++ {
		if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
			t.Fatal(err)
		}
	}
	assertRetainedActorCapture(t, f, req.Workspace.Captured, f.rootID)
	var status string
	var pin *bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT lease_status,availability_required FROM run_finalization_objects WHERE run_lease_id=$1`, f.claim.runLease.ID).Scan(&status, &pin); err != nil || status != "failed" || pin != nil {
		t.Fatalf("registration release=%s %v %v", status, pin, err)
	}
	// The lease pin is released, but committed CAS membership still forbids deletion.
	if _, err := db.New(f.Pool).RetireAbandonedCasObject(t.Context(), obj.Digest); err == nil {
		t.Fatal("retired committed result disk")
	}
}

func TestRunFinalizationRegistrationRejectsExpiredAuthorityPostgres(t *testing.T) {
	f, req := finalizingActorRequest(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()+interval '50 milliseconds' WHERE id=$1`, f.claim.runLease.ID)
	dbtest.MustExec(t, t.Context(), f.Pool, `SELECT pg_sleep(0.1)`)
	registration := workerapi.RegisterRunFinalizationRequest{Lease: req.Lease, OperationID: req.Workspace.Captured.Receipt.OperationID, Disk: req.Workspace.Captured.Disk}
	if err := f.server.registerRunFinalization(t.Context(), f.worker, registration); err == nil {
		t.Fatal("expired registration accepted")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_finalization_objects WHERE run_lease_id=$1`, pgvalue.UUID(uuid.MustParse(req.Lease.ID))).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired candidate retained %d %v", count, err)
	}
}

func finalizationActorStatus(t *testing.T, f *actorCheckpointFixture, req workerapi.CompleteActorRequest) int {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/worker/v1/run/actors/complete", bytes.NewReader(body)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
	response := httptest.NewRecorder()
	f.server.workerCompleteActor(response, r)
	return response.Code
}

func TestRunFinalizationPublicationExpiresDuringMembershipWritePostgres(t *testing.T) {
	f, req := finalizingActorRequest(t)
	f.registerFinalizationDisk(t, req.Workspace.Captured, "expiry while blocked")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	locker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, locker, `SELECT digest FROM cas_object_lifetimes WHERE digest=$1 FOR UPDATE`, req.Workspace.Captured.Disk.Artifact.Digest)
	var expiry time.Time
	if err := f.Pool.QueryRow(ctx, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1 RETURNING expires_at`, f.claim.runLease.ID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE workspace_leases SET expires_at=$2 WHERE id=$1`, f.claim.workspaceLease.ID, expiry)
	req.Workspace.Captured.Receipt.Fence.ExpiresAt = expiry
	setCaptureFingerprint(t, req.Workspace.Captured)
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE run_leases SET finalization_request_fingerprint=$2 WHERE id=$1`, f.claim.runLease.ID, req.Workspace.Captured.Receipt.RequestFingerprint)
	parsed, err := parseActorCompletionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- f.server.completeActor(ctx, f.worker, req, parsed) }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, locker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("did not reach membership lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-time.After(time.Until(expiry) + 50*time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, errStaleActorCompletion) {
		t.Fatalf("expired publication=%v", err)
	}
	var head uuid.UUID
	var status string
	var memberships int
	if err := f.Pool.QueryRow(ctx, `SELECT w.head_version_id,l.status,(SELECT count(*) FROM cas_objects WHERE digest=$3) FROM computers w JOIN run_leases l ON l.id=$2 WHERE w.id=$1`, f.workspaceID, f.claim.runLease.ID, req.Workspace.Captured.Disk.Artifact.Digest).Scan(&head, &status, &memberships); err != nil {
		t.Fatal(err)
	}
	if head != f.rootID || status != "finalizing" || memberships != 0 {
		t.Fatalf("partial publication head=%s status=%s memberships=%d", head, status, memberships)
	}
}

func TestTaskFinalizationRejectsUnregisteredDiskPostgres(t *testing.T) {
	f := newSameWorkspaceCompletionPostgresFixture(t, false)
	changed := f.request
	changed.Workspace.Captured = cloneTaskWorkspaceCapture(f.request.Workspace.Captured)
	other, err := f.server.cas.Put(t.Context(), computer.DiskMediaType, strings.NewReader("unregistered task disk"))
	if err != nil {
		t.Fatal(err)
	}
	changed.Workspace.Captured.Disk.Artifact = workerapi.CheckpointArtifact{Digest: other.Digest, SizeBytes: other.SizeBytes, MediaType: other.MediaType}
	body, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	f.server.log = slog.Default()
	f.server.workerCompleteTask(response, httptest.NewRequest(http.MethodPost, "/worker/v1/run/tasks/complete", bytes.NewReader(body)).WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker)))
	if response.Code != http.StatusConflict {
		t.Fatalf("unregistered task completion=%d %s", response.Code, response.Body.String())
	}
	var status string
	if err := f.pool.QueryRow(t.Context(), `SELECT status FROM run_leases WHERE id=$1`, uuid.MustParse(changed.Lease.ID)).Scan(&status); err != nil || status != "finalizing" {
		t.Fatalf("unregistered task changed authority=%s %v", status, err)
	}
}
