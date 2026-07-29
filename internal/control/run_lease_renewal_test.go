package control

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRenewRunLeaseReplaysOnlyTheImmediatelyPreviousExpiry(t *testing.T) {
	server, store, worker, first := validRunLeaseRenewalFixture(t)

	second, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, first.Fence(), first.ExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) || store.renewalWrites != 2 {
		t.Fatalf("first renewal = %+v, writes = %d", second, store.renewalWrites)
	}
	wantCalls := []string{
		"renewal_locators", "run", "workspace", "attempt", "worker_group", "worker",
		"network_slot", "runtime", "renewal_lease", "workspace_mount", "workspace_lease",
		"renewal_time", "renew_run_lease", "renew_workspace_lease", "commit",
	}
	if !slices.Equal(store.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", store.calls, wantCalls)
	}
	store.calls = nil
	replayed, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, first.Fence(), first.ExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != second || store.renewalWrites != 2 {
		t.Fatalf("replay = %+v, writes = %d", replayed, store.renewalWrites)
	}
	if slices.Contains(store.calls, "renew_run_lease") || slices.Contains(store.calls, "renew_workspace_lease") {
		t.Fatalf("replay changed authority: %v", store.calls)
	}

	store.renewalTime.Time = store.renewalTime.Time.Add(time.Minute)
	third, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, second.Lease, second.ExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !third.ExpiresAt.After(second.ExpiresAt) || store.renewalWrites != 4 {
		t.Fatalf("second renewal = %+v, writes = %d", third, store.renewalWrites)
	}
	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, first.Fence(), first.ExpiresAt,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("two-generation-old expiry error = %v, want stale", err)
	}
}

func TestRenewRunLeaseRejectsUnexpectedExpiry(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	expectedExpiry := assignment.ExpiresAt.Add(time.Second)
	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), expectedExpiry,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale", err)
	}
	if store.renewalWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.renewalWrites)
	}
}

func TestRenewRunLeaseRejectsStaleSequence(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	assignment.LeaseSequence++
	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale", err)
	}
}

func TestRenewRunLeaseRejectsPriorWorkerEpoch(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	worker.WorkerEpoch++
	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale", err)
	}
}

func TestRenewRunLeaseAllowsDrainingOwner(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	store.authority.workerGroup.State = db.WorkerGroupStateDraining
	store.authority.worker.State = db.WorkerInstanceStateDraining

	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestRenewRunLeaseUsesRestoredPhysicalFrontier(t *testing.T) {
	server, store, worker, _ := validRunLeaseRenewalFixture(t)
	restored := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if restored == store.authority.attempt.BaseWorkspaceVersionID {
		t.Fatal("restored frontier unexpectedly matches the Attempt base")
	}
	store.authority.workspaceLease.BaseVersionID = restored
	store.authority.workspaceMount.MaterializedVersionID = restored
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
		run: store.authority.run, attempt: store.authority.attempt, runtime: store.authority.runtime,
		networkSlot: store.authority.networkSlot, runLease: store.authority.runLease,
		workspace: store.authority.workspace, workspaceMount: store.authority.workspaceMount,
		workspaceLease: store.authority.workspaceLease,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestRenewRunLeaseReturnsCurrentExpiryWhenOperationalHorizonDoesNotAdvance(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	store.authority.runLease.ExpiresAt.Time = store.renewalTime.Time.Add(server.runLeaseTTL)
	store.authority.workspaceLease.ExpiresAt = store.authority.runLease.ExpiresAt
	assignment.ExpiresAt = store.authority.runLease.ExpiresAt.Time

	renewed, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := api.WorkerRunLeaseRenewResponse{
		Lease: assignment.Fence(), ExpiresAt: assignment.ExpiresAt,
		BaseWorkspaceVersionID: assignment.BaseWorkspaceVersionID,
	}
	if renewed != want || store.renewalWrites != 0 {
		t.Fatalf("renewed = %+v, writes = %d", renewed, store.renewalWrites)
	}
}

func TestRenewRunLeaseUsesConfiguredOperationalHorizon(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	server.runLeaseTTL = 2 * time.Minute

	renewed, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := store.renewalTime.Time.Add(server.runLeaseTTL)
	if !renewed.ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %s, want %s", renewed.ExpiresAt, want)
	}
}

func TestRenewRunLeaseRejectsAuthorityThatExpiredWhileWaitingForLocks(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	store.renewalTime.Time = store.authority.runLease.ExpiresAt.Time

	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale", err)
	}
	if store.renewalWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.renewalWrites)
	}
}

func TestRenewRunLeaseRejectsExhaustedActiveBudget(t *testing.T) {
	server, store, worker, assignment := validRunLeaseRenewalFixture(t)
	store.authority.run.ActiveStartedAt.Time = store.renewalTime.Time.Add(-time.Hour)
	store.authority.run.MaxActiveDurationMs = int64(time.Hour / time.Millisecond)
	store.authority.run.ActiveElapsedMs = 0
	if _, err := server.renewRunLease(
		context.Background(), worker, store.authority.runLease.ID, assignment.Fence(), assignment.ExpiresAt,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale", err)
	}
	if store.renewalWrites != 0 {
		t.Fatalf("writes = %d, want zero", store.renewalWrites)
	}
}

func validRunLeaseRenewalFixture(
	t *testing.T,
) (*Server, *runLeaseClaimStore, workerActor, api.WorkerRunLeaseAssignment) {
	t.Helper()
	worker, claimLocators, authority := validRunLeaseClaimFixture()
	now := time.Now().UTC().Truncate(time.Microsecond)
	authority.run.Status = db.RunStatusRunning
	authority.run.StartedAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.run.ActiveStartedAt = authority.run.StartedAt
	authority.run.MaxActiveDurationMs = int64(time.Hour / time.Millisecond)
	authority.run.ActiveElapsedMs = 0
	authority.runLease.State = db.RunLeaseStateRunning
	authority.runLease.StartDeadlineAt = pgvalue.Timestamptz(now.Add(-30 * time.Second))
	authority.runLease.StartedAt = authority.run.StartedAt
	authority.runLease.ExpiresAt = pgvalue.Timestamptz(now.Add(time.Minute))
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.ExpiresAt = authority.runLease.ExpiresAt

	store := &runLeaseClaimStore{
		authority: authority,
		renewal: db.GetLiveRunLeaseLocatorsRow{
			OrgID: claimLocators.OrgID, ProjectID: claimLocators.ProjectID,
			EnvironmentID: claimLocators.EnvironmentID, RunID: claimLocators.RunID,
			WorkspaceID: claimLocators.WorkspaceID, AttemptNumber: claimLocators.AttemptNumber,
			RegionID: claimLocators.RegionID, RuntimeInstanceID: claimLocators.RuntimeInstanceID,
			NetworkSlotID:         claimLocators.NetworkSlotID,
			NetworkSlotGeneration: claimLocators.NetworkSlotGeneration,
			WorkspaceLeaseID:      claimLocators.WorkspaceLeaseID,
			WorkspaceMountID:      claimLocators.WorkspaceMountID,
		},
		renewalTime: pgvalue.Timestamptz(now),
	}
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{db: store, runLeaseTTL: 5 * time.Minute}, store, worker, assignment
}

func (s *runLeaseClaimStore) GetLiveRunLeaseLocators(
	_ context.Context,
	params db.GetLiveRunLeaseLocatorsParams,
) (db.GetLiveRunLeaseLocatorsRow, error) {
	s.calls = append(s.calls, "renewal_locators")
	lease := s.authority.runLease
	if params.ID != lease.ID ||
		params.LeaseSequence != lease.LeaseSequence ||
		params.WorkerGroupID != lease.WorkerGroupID ||
		params.WorkerInstanceID != lease.WorkerInstanceID ||
		params.WorkerEpoch != lease.WorkerEpoch ||
		params.WorkerProtocolVersion != lease.WorkerProtocolVersion {
		return db.GetLiveRunLeaseLocatorsRow{}, pgx.ErrNoRows
	}
	return s.renewal, nil
}

func (s *runLeaseClaimStore) LockLiveRunLease(
	context.Context,
	db.LockLiveRunLeaseParams,
) (db.RunLease, error) {
	s.calls = append(s.calls, "renewal_lease")
	return s.authority.runLease, nil
}

func (s *runLeaseClaimStore) GetRunLeaseRenewalTime(context.Context) (pgtype.Timestamptz, error) {
	s.calls = append(s.calls, "renewal_time")
	return s.renewalTime, nil
}

func (s *runLeaseClaimStore) RenewRunLeaseExpiry(
	_ context.Context,
	params db.RenewRunLeaseExpiryParams,
) (db.RunLease, error) {
	s.calls = append(s.calls, "renew_run_lease")
	if !s.authority.runLease.ExpiresAt.Time.Equal(params.PreviousExpiresAt.Time) {
		return db.RunLease{}, errStaleRunLeaseClaim
	}
	s.authority.runLease.PreviousExpiresAt = s.authority.runLease.ExpiresAt
	s.authority.runLease.RenewedAt = params.RenewedAt
	s.authority.runLease.ExpiresAt = params.ExpiresAt
	s.renewalWrites++
	return s.authority.runLease, nil
}

func (s *runLeaseClaimStore) RenewRunWorkspaceLeaseExpiry(
	_ context.Context,
	params db.RenewRunWorkspaceLeaseExpiryParams,
) (db.WorkspaceLease, error) {
	s.calls = append(s.calls, "renew_workspace_lease")
	if !s.authority.workspaceLease.ExpiresAt.Time.Equal(params.PreviousExpiresAt.Time) {
		return db.WorkspaceLease{}, errStaleRunLeaseClaim
	}
	s.authority.workspaceLease.RenewedAt = params.RenewedAt
	s.authority.workspaceLease.ExpiresAt = params.ExpiresAt
	s.renewalWrites++
	return s.authority.workspaceLease, nil
}
