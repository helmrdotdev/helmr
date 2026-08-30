package dispatch

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspaceExecClaimRechecksStartingAdmissionButPreservesContinuation(t *testing.T) {
	for _, test := range []struct {
		name         string
		processState db.WorkspaceProcessState
		wantLock     bool
	}{
		{
			name:         "starting is rejected after group drain",
			processState: db.WorkspaceProcessStateStarting,
			wantLock:     false,
		},
		{
			name:         "running can continue after group drain",
			processState: db.WorkspaceProcessStateRunning,
			wantLock:     true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunPlacementFixture(t)
			processID, mountID := placeWorkspaceExecForClaim(t, fixture)
			if test.processState == db.WorkspaceProcessStateRunning {
				if _, err := db.New(fixture.pool).StartWorkspaceExec(
					fixture.ctx,
					db.StartWorkspaceExecParams{
						ProcessID:        pgvalue.UUID(processID),
						WorkspaceMountID: pgvalue.UUID(mountID),
					},
				); err != nil {
					t.Fatal(err)
				}
			}
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_groups
   SET state = 'draining'
 WHERE id = $1`,
				fixture.groupID,
			)

			authority, err := db.New(fixture.pool).LockWorkspaceExecWorkerAuthority(
				fixture.ctx,
				db.LockWorkspaceExecWorkerAuthorityParams{
					OrgID:                       pgvalue.UUID(fixture.orgID),
					ProcessID:                   pgvalue.UUID(processID),
					WorkspaceMountID:            pgvalue.UUID(mountID),
					WorkerInstanceID:            pgvalue.UUID(fixture.workerID),
					WorkerEpoch:                 1,
					ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
				},
			)
			if test.wantLock {
				if err != nil {
					t.Fatal(err)
				}
				if authority.WorkspaceProcess.State != test.processState {
					t.Fatalf(
						"process state = %q, want %q",
						authority.WorkspaceProcess.State,
						test.processState,
					)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("authority lock error = %v, want pgx.ErrNoRows", err)
			}
		})
	}
}

func placeWorkspaceExecForClaim(
	t *testing.T,
	fixture runPlacementFixture,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE workspaces
   SET owner_run_id = NULL
 WHERE id = $1`,
		fixture.workspaceID,
	)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', decode(repeat('51', 32), 'hex'),
    decode(repeat('52', 32), 'hex'), now()
)`,
		claimID,
		fixture.environmentID,
	)
	if _, err := db.New(fixture.pool).CreateWorkspaceExec(
		fixture.ctx,
		db.CreateWorkspaceExecParams{
			ID:                   pgvalue.UUID(processID),
			OrgID:                pgvalue.UUID(fixture.orgID),
			ProjectID:            pgvalue.UUID(fixture.projectID),
			EnvironmentID:        pgvalue.UUID(fixture.environmentID),
			WorkspaceID:          pgvalue.UUID(fixture.workspaceID),
			BaseVersionID:        workspaceHeadVersion(t, fixture),
			RestoreDesiredState:  "active",
			Request:              []byte(`{"command":["echo","ready"]}`),
			Stdin:                []byte{},
			ClaimID:              pgvalue.UUID(claimID),
			CreatedBySubjectType: "user",
			CreatedBySubjectID:   "test-user",
		},
	); err != nil {
		t.Fatal(err)
	}
	candidate := ReadyWorkspaceExecCandidate{
		OrgID:                pgvalue.UUID(fixture.orgID),
		ProcessID:            pgvalue.UUID(processID),
		ExpectedStateVersion: 1,
	}
	reserved, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !reserved.RuntimeInstanceID.Valid {
		t.Fatalf("runtime reservation = %+v", reserved)
	}
	if reserved.ProcessBound {
		if !reserved.WorkspaceMountID.Valid {
			t.Fatalf("warm placement = %+v", reserved)
		}
		return processID, pgvalue.MustUUIDValue(reserved.WorkspaceMountID)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount placement = %+v", mounting)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	bound, err := fixture.authority.PlaceWorkspaceExec(fixture.ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !bound.ProcessBound {
		t.Fatalf("bound placement = %+v", bound)
	}
	return processID, pgvalue.MustUUIDValue(bound.WorkspaceMountID)
}

func workspaceHeadVersion(t *testing.T, fixture runPlacementFixture) pgtype.UUID {
	t.Helper()
	var versionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT head_version_id
  FROM workspaces
 WHERE id = $1`,
		fixture.workspaceID,
	).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if !versionID.Valid {
		t.Fatal("Workspace head version is missing")
	}
	return versionID
}
