package controlplane

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

type workspaceDeletionPhysicalFixture struct {
	base             runtest.Fixture
	work             runtest.RunLease
	workspaceID      uuid.UUID
	runtimeID        uuid.UUID
	mountID          uuid.UUID
	workspaceLeaseID uuid.UUID
}

func newWorkspaceDeletionPhysicalFixture(t *testing.T) workspaceDeletionPhysicalFixture {
	t.Helper()
	base := runtest.New(t)
	work := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	fixture := workspaceDeletionPhysicalFixture{base: base, work: work}
	if err := base.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, run_leases.runtime_instance_id,
       workspace_leases.workspace_mount_id, workspace_leases.id
  FROM runs
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.RunID).Scan(
		&fixture.workspaceID, &fixture.runtimeID, &fixture.mountID, &fixture.workspaceLeaseID,
	); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f workspaceDeletionPhysicalFixture) setDeleting(t *testing.T, clearOwner bool) {
	t.Helper()
	if clearOwner {
		dbtest.MustExec(t, t.Context(), f.base.Pool, `
UPDATE workspaces
   SET state = 'deleting', desired_state = 'deleted', owner_run_id = NULL
 WHERE id = $1`, f.workspaceID)
		return
	}
	dbtest.MustExec(t, t.Context(), f.base.Pool, `
UPDATE workspaces SET state = 'deleting', desired_state = 'deleted'
 WHERE id = $1`, f.workspaceID)
}

func (f workspaceDeletionPhysicalFixture) releaseLease(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.base.Pool, `
UPDATE workspace_leases
   SET state = 'released', released_at = now(), terminal_at = now()
 WHERE id = $1`, f.workspaceLeaseID)
}

func (f workspaceDeletionPhysicalFixture) unmount(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.base.Pool, `
UPDATE workspace_mounts
   SET state = 'unmounted', unmounted_at = now(), terminal_at = now(),
       terminal_reason_code = 'test_unmounted'
 WHERE id = $1`, f.mountID)
}

func (f workspaceDeletionPhysicalFixture) loseRuntime(t *testing.T) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.base.Pool, `
UPDATE runtime_instances
   SET observed_state = 'lost', lost_at = now(), terminal_at = now(),
       terminal_reason_code = 'test_lost'
 WHERE id = $1`, f.runtimeID)
}

func (f workspaceDeletionPhysicalFixture) finalize(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := db.New(f.base.Pool).FinalizeDeletingWorkspaces(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, len(rows))
	for index, row := range rows {
		ids[index] = row.Bytes
	}
	return ids
}

func TestWorkspaceDeleteFinalizationAuthorityBlockers(t *testing.T) {
	t.Run("run owner", func(t *testing.T) {
		fixture := newWorkspaceDeletionPhysicalFixture(t)
		fixture.releaseLease(t)
		fixture.unmount(t)
		fixture.loseRuntime(t)
		fixture.setDeleting(t, false)
		if rows := fixture.finalize(t); len(rows) != 0 {
			t.Fatalf("owner-blocked finalization = %v", rows)
		}
		dbtest.MustExec(t, t.Context(), fixture.base.Pool,
			`UPDATE workspaces SET owner_run_id = NULL WHERE id = $1`, fixture.workspaceID)
		if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
			t.Fatalf("owner-released finalization = %v", rows)
		}
	})

	t.Run("session owner", func(t *testing.T) {
		fixture := newWorkspaceDeletionPhysicalFixture(t)
		fixture.base.ConvertToActor(t, t.Context(), fixture.work, `{"enabled":false}`)
		fixture.releaseLease(t)
		fixture.unmount(t)
		fixture.loseRuntime(t)
		fixture.setDeleting(t, false)
		if rows := fixture.finalize(t); len(rows) != 0 {
			t.Fatalf("session-owner-blocked finalization = %v", rows)
		}
		dbtest.MustExec(t, t.Context(), fixture.base.Pool,
			`UPDATE workspaces SET owner_session_id = NULL WHERE id = $1`, fixture.workspaceID)
		if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
			t.Fatalf("session-owner-released finalization = %v", rows)
		}
	})

	for _, state := range []string{"active", "releasing"} {
		t.Run("workspace lease "+state, func(t *testing.T) {
			fixture := newWorkspaceDeletionPhysicalFixture(t)
			if state == "releasing" {
				dbtest.MustExec(t, t.Context(), fixture.base.Pool,
					`UPDATE workspace_leases SET state = 'releasing' WHERE id = $1`, fixture.workspaceLeaseID)
			}
			fixture.unmount(t)
			fixture.loseRuntime(t)
			fixture.setDeleting(t, true)
			if rows := fixture.finalize(t); len(rows) != 0 {
				t.Fatalf("%s lease-blocked finalization = %v", state, rows)
			}
			fixture.releaseLease(t)
			if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
				t.Fatalf("lease-released finalization = %v", rows)
			}
		})
	}

	for _, state := range []string{"mounting", "mounted", "unmounting"} {
		t.Run("workspace mount "+state, func(t *testing.T) {
			fixture := newWorkspaceDeletionPhysicalFixture(t)
			fixture.releaseLease(t)
			fixture.loseRuntime(t)
			dbtest.MustExec(t, t.Context(), fixture.base.Pool,
				`UPDATE workspace_mounts SET state = $2 WHERE id = $1`, fixture.mountID, state)
			fixture.setDeleting(t, true)
			if rows := fixture.finalize(t); len(rows) != 0 {
				t.Fatalf("%s mount-blocked finalization = %v", state, rows)
			}
			fixture.unmount(t)
			if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
				t.Fatalf("mount-released finalization = %v", rows)
			}
		})
	}

	t.Run("unreclaimed failed runtime", func(t *testing.T) {
		fixture := newWorkspaceDeletionPhysicalFixture(t)
		fixture.releaseLease(t)
		fixture.unmount(t)
		dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', failed_at = now(), terminal_at = now(),
       terminal_reason_code = 'test_failed'
 WHERE id = $1`, fixture.runtimeID)
		fixture.setDeleting(t, true)
		if rows := fixture.finalize(t); len(rows) != 0 {
			t.Fatalf("failed-runtime-blocked finalization = %v", rows)
		}
		dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
UPDATE runtime_instances
   SET reclaimed_at = now(), reclaim_evidence = '{"method":"test"}'::jsonb
 WHERE id = $1`, fixture.runtimeID)
		if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
			t.Fatalf("runtime-reclaimed finalization = %v", rows)
		}
	})

	t.Run("workspace process states", func(t *testing.T) {
		fixture := newWorkspaceDeletionPhysicalFixture(t)
		fixture.releaseLease(t)
		fixture.unmount(t)
		fixture.loseRuntime(t)
		fixture.setDeleting(t, true)
		claimID := uuid.Must(uuid.NewV7())
		processID := uuid.Must(uuid.NewV7())
		var versionID uuid.UUID
		if err := fixture.base.Pool.QueryRow(t.Context(), `
SELECT head_version_id FROM workspaces WHERE id = $1`, fixture.workspaceID).Scan(&versionID); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint, accepted_at
) VALUES ($1, $2, 'workspace.exec', decode(repeat('61', 32), 'hex'),
          decode(repeat('62', 32), 'hex'), now())`, claimID, fixture.base.EnvironmentID)
		dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
INSERT INTO workspace_processes (
    id, org_id, project_id, environment_id, workspace_id, base_version_id,
    restore_desired_state, request, claim_id, created_by_subject_type,
    created_by_subject_id
) VALUES ($1, $2, $3, $4, $5, $6, 'active', '{}'::jsonb, $7, 'test', 'test')`,
			processID, fixture.base.OrgID, fixture.base.ProjectID,
			fixture.base.EnvironmentID, fixture.workspaceID, versionID, claimID)
		if rows := fixture.finalize(t); len(rows) != 0 {
			t.Fatalf("pending-process-blocked finalization = %v", rows)
		}
		for _, state := range []string{"starting", "running", "exit_requested"} {
			dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
UPDATE workspace_processes
   SET state = $2, region_id = $3, worker_group_id = $4,
       worker_instance_id = $5, worker_epoch = 1, runtime_instance_id = $6,
       workspace_mount_id = $7,
       stdout = CASE WHEN $2 = 'exit_requested' THEN ''::bytea ELSE stdout END,
       stderr = CASE WHEN $2 = 'exit_requested' THEN ''::bytea ELSE stderr END
 WHERE id = $1`, processID, state, runtest.Region, runtest.WorkerGroup,
				fixture.base.WorkerID, fixture.runtimeID, fixture.mountID)
			if rows := fixture.finalize(t); len(rows) != 0 {
				t.Fatalf("%s-process-blocked finalization = %v", state, rows)
			}
		}
		dbtest.MustExec(t, t.Context(), fixture.base.Pool, `
UPDATE workspace_processes
   SET state = 'failed', terminal_at = now(), terminal_reason_code = 'test_failed'
 WHERE id = $1`, processID)
		if rows := fixture.finalize(t); len(rows) != 1 || rows[0] != fixture.workspaceID {
			t.Fatalf("process-terminal finalization = %v", rows)
		}
	})
}
