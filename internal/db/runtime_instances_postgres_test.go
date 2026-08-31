package db

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestRuntimeInstanceAllocatedReadyClosedPath(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	runtime := loadRuntimeLease(t, fixture, work)
	detachRuntimeLease(t, fixture, work, runtime.id)
	resetRuntimeAllocated(t, fixture, runtime.id)
	substrateID := seedRuntimeSubstrate(t, fixture, runtime.definitionID)

	ready, err := queries.MarkRuntimeInstanceReady(ctx, MarkRuntimeInstanceReadyParams{
		RuntimeSubstrateID:      pgvalue.UUID(substrateID),
		DesiredVersion:          1,
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		ExpectedObservedVersion: 0,
		VMVCPUCount:             1,
		CPUConfigDigest:         fixture.CPUConfigDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.ObservedState != RuntimeObservedStateReady || ready.ObservedVersion != 1 ||
		ready.ObservedDesiredVersion != 1 || !ready.ReadyAt.Valid || ready.TerminalAt.Valid ||
		ready.ReclaimedAt.Valid {
		t.Fatalf("ready runtime = %+v", ready)
	}

	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = 2,
       desired_reason = 'test_closed', updated_at = now()
 WHERE id = $1`, runtime.id)

	closed, err := queries.MarkRuntimeInstanceClosed(ctx, MarkRuntimeInstanceClosedParams{
		ReasonCode:              pgvalue.Text("test_closed"),
		CleanupProof:            []byte(`{"method":"host_reconciled","completed_at":"2026-08-31T00:00:00Z"}`),
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		DesiredVersion:          2,
		ExpectedObservedVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.ObservedState != RuntimeObservedStateClosed || closed.ObservedVersion != 2 ||
		closed.ObservedDesiredVersion != 2 || !closed.ReadyAt.Valid || !closed.TerminalAt.Valid ||
		!closed.ReclaimedAt.Valid || closed.TerminalError != nil || closed.ReservedRunID.Valid {
		t.Fatalf("closed runtime = %+v", closed)
	}
}

func TestRuntimeInstanceFailureFromAllocatedAndReady(t *testing.T) {
	ctx := t.Context()
	for _, start := range []string{"allocated", "ready"} {
		t.Run(start, func(t *testing.T) {
			fixture := runtest.New(t)
			queries := New(fixture.Pool)
			work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
			runtime := loadRuntimeLease(t, fixture, work)
			detachRuntimeLease(t, fixture, work, runtime.id)
			expectedObserved := int64(1)
			if start == "allocated" {
				resetRuntimeAllocated(t, fixture, runtime.id)
				expectedObserved = 0
			}

			failed, err := queries.MarkRuntimeInstanceFailed(ctx, MarkRuntimeInstanceFailedParams{
				ReasonCode:              pgvalue.Text("test_failed"),
				Error:                   []byte(`{"code":"runtime_failed"}`),
				ID:                      pgvalue.UUID(runtime.id),
				WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
				WorkerEpoch:             1,
				DesiredVersion:          1,
				ExpectedObservedVersion: expectedObserved,
			})
			if err != nil {
				t.Fatal(err)
			}
			if failed.ObservedState != RuntimeObservedStateFailed ||
				failed.ObservedVersion != expectedObserved+1 || !failed.TerminalAt.Valid ||
				failed.ReclaimedAt.Valid || !bytes.Contains(failed.TerminalError, []byte(`"runtime_failed"`)) {
				t.Fatalf("failed from %s = %+v", start, failed)
			}
			if start == "allocated" && failed.ReadyAt.Valid {
				t.Fatalf("allocated failure wrote ready_at: %+v", failed)
			}
			if start == "ready" && !failed.ReadyAt.Valid {
				t.Fatalf("ready failure dropped ready_at: %+v", failed)
			}

			reclaimed, err := queries.ReclaimFailedRuntimeInstance(ctx, ReclaimFailedRuntimeInstanceParams{
				CleanupProof:            []byte(`{"method":"host_reconciled","completed_at":"2026-08-31T00:00:00Z"}`),
				ID:                      pgvalue.UUID(runtime.id),
				WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
				WorkerEpoch:             1,
				DesiredVersion:          1,
				ExpectedObservedVersion: failed.ObservedVersion,
			})
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed.ObservedState != RuntimeObservedStateFailed || !reclaimed.ReclaimedAt.Valid ||
				!reclaimed.TerminalAt.Valid {
				t.Fatalf("reclaimed failed runtime = %+v", reclaimed)
			}
		})
	}
}

func TestRuntimeInstanceStaleFencesAreRejected(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	runtime := loadRuntimeLease(t, fixture, work)
	detachRuntimeLease(t, fixture, work, runtime.id)
	resetRuntimeAllocated(t, fixture, runtime.id)
	substrateID := seedRuntimeSubstrate(t, fixture, runtime.definitionID)
	params := MarkRuntimeInstanceReadyParams{
		RuntimeSubstrateID:      pgvalue.UUID(substrateID),
		DesiredVersion:          1,
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		ExpectedObservedVersion: 0,
		VMVCPUCount:             1,
		CPUConfigDigest:         fixture.CPUConfigDigest,
	}

	staleEpoch := params
	staleEpoch.WorkerEpoch = 2
	if _, err := queries.MarkRuntimeInstanceReady(ctx, staleEpoch); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale epoch error = %v, want pgx.ErrNoRows", err)
	}

	staleObserved := params
	staleObserved.ExpectedObservedVersion = 3
	if _, err := queries.MarkRuntimeInstanceReady(ctx, staleObserved); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale observed version error = %v, want pgx.ErrNoRows", err)
	}

	staleDesired := params
	staleDesired.DesiredVersion = 4
	if _, err := queries.MarkRuntimeInstanceReady(ctx, staleDesired); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale desired version error = %v, want pgx.ErrNoRows", err)
	}

	ready, err := queries.MarkRuntimeInstanceReady(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if ready.ObservedVersion != 1 {
		t.Fatalf("current fence ready version = %d", ready.ObservedVersion)
	}
}

func TestRuntimeInstanceReservationMisuseIsRejected(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	runtime := loadRuntimeLease(t, fixture, work)
	_, err := fixture.Pool.Exec(ctx, `
UPDATE runtime_instances
   SET reserved_run_id = $2, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id, work.RunID)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("partial reservation error = %v, want check constraint", err)
	}

	var baseWorkspaceVersionID uuid.UUID
	if err := fixture.Pool.QueryRow(ctx, `
SELECT base_workspace_version_id FROM runs WHERE id = $1`, work.RunID,
	).Scan(&baseWorkspaceVersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = 2, desired_reason = 'test_closed',
       observed_state = 'closed', observed_version = 2, observed_desired_version = 2,
       terminal_at = now(), terminal_reason_code = 'test_closed',
       reclaimed_at = now(), reclaim_evidence = '{"method":"test"}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	_, err = fixture.Pool.Exec(ctx, `
UPDATE runtime_instances
   SET reserved_run_id = $2, reserved_attempt_number = 1,
       reserved_workspace_version_id = $3,
       reservation_expires_at = now() + interval '5 minutes'
 WHERE id = $1`, runtime.id, work.RunID, baseWorkspaceVersionID)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("closed reservation error = %v, want check constraint", err)
	}
}

func TestRuntimeInstanceLeaseRecoveryAliasesAreStateConditioned(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		state      string
		wantFailed bool
		wantLost   bool
		reason     string
	}{
		{state: "failed", wantFailed: true, reason: "test_runtime_failed"},
		{state: "lost", wantLost: true, reason: "test_worker_lost"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fixture := runtest.New(t)
			work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
			runtime := loadRuntimeLease(t, fixture, work)
			dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = $2, observed_version = observed_version + 1,
       terminal_at = now(), terminal_reason_code = $3,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id, tc.state, tc.reason)

			row, err := New(fixture.Pool).GetRunExecutionLeaseLossAuthority(ctx, GetRunExecutionLeaseLossAuthorityParams{
				RunID:         pgvalue.UUID(work.RunID),
				WorkspaceID:   pgvalue.UUID(runtime.workspaceID),
				AttemptNumber: 1,
				RunLeaseID:    pgvalue.UUID(work.LeaseID),
			})
			if err != nil {
				t.Fatal(err)
			}
			if row.RuntimeObservedState != tc.state ||
				row.RuntimeFailedAt.Valid != tc.wantFailed ||
				row.RuntimeLostAt.Valid != tc.wantLost {
				t.Fatalf("recovery aliases = state:%s failed:%v lost:%v",
					row.RuntimeObservedState, row.RuntimeFailedAt.Valid, row.RuntimeLostAt.Valid)
			}
			if tc.wantFailed && (!row.RuntimeFailedAt.Valid || row.RuntimeLostAt.Valid) {
				t.Fatalf("failed aliases filled both timestamps: %+v", row)
			}
			if tc.wantLost && (!row.RuntimeLostAt.Valid || row.RuntimeFailedAt.Valid) {
				t.Fatalf("lost aliases filled both timestamps: %+v", row)
			}
		})
	}
}

func TestRuntimeInstanceRowShapes(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	runtime := loadRuntimeLease(t, fixture, work)

	assertRejected := func(name, sql string, args ...any) {
		t.Helper()
		_, err := fixture.Pool.Exec(ctx, sql, args...)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s error = %v, want check constraint", name, err)
		}
	}

	assertRejected("allocated with terminal", `
UPDATE runtime_instances
   SET observed_state = 'allocated', ready_at = NULL, terminal_at = now(),
       terminal_reason_code = 'too_early'
 WHERE id = $1`, runtime.id)
	assertRejected("ready without ready_at", `
UPDATE runtime_instances
   SET observed_state = 'ready', ready_at = NULL
 WHERE id = $1`, runtime.id)
	assertRejected("closed without reclaim", `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = 2, desired_reason = 'test_closed',
       observed_state = 'closed', observed_desired_version = 2,
       terminal_at = now(), terminal_reason_code = 'test_closed',
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	assertRejected("closed with terminal error", `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = 2, desired_reason = 'test_closed',
       observed_state = 'closed', observed_desired_version = 2,
       terminal_at = now(), terminal_reason_code = 'test_closed',
       terminal_error = '{"code":"nope"}'::jsonb,
       reclaimed_at = now(), reclaim_evidence = '{"method":"test"}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	assertRejected("failed without reason", `
UPDATE runtime_instances
   SET observed_state = 'failed', terminal_at = now(), terminal_reason_code = NULL,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	assertRejected("reclaim evidence without timestamp", `
UPDATE runtime_instances
   SET observed_state = 'failed', terminal_at = now(), terminal_reason_code = 'test_failed',
       reclaim_evidence = '{"method":"test"}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	assertRejected("reclaim before terminal", `
UPDATE runtime_instances
   SET observed_state = 'failed', terminal_at = now(), terminal_reason_code = 'test_failed',
       reclaimed_at = now() - interval '1 second',
       reclaim_evidence = '{"method":"test"}'::jsonb,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)

	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'allocated', ready_at = NULL,
       reserved_run_id = NULL, reserved_attempt_number = NULL,
       reserved_workspace_version_id = NULL, reservation_expires_at = NULL
 WHERE id = $1`, runtime.id)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'ready', ready_at = now()
 WHERE id = $1`, runtime.id)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', terminal_at = now(), terminal_reason_code = 'test_failed',
       terminal_error = '{"code":"ok"}'::jsonb
 WHERE id = $1`, runtime.id)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET reclaimed_at = now(), reclaim_evidence = '{"method":"test"}'::jsonb
 WHERE id = $1`, runtime.id)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'lost', terminal_reason_code = 'test_lost'
 WHERE id = $1`, runtime.id)
}

type runtimeLease struct {
	id           uuid.UUID
	workspaceID  uuid.UUID
	definitionID uuid.UUID
}

func loadRuntimeLease(t *testing.T, fixture runtest.Fixture, work runtest.RunLease) runtimeLease {
	t.Helper()
	var runtime runtimeLease
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runtime_instances.id, runtime_instances.workspace_id,
       runtime_instances.deployment_definition_id
  FROM runtime_instances
  JOIN run_leases ON run_leases.runtime_instance_id = runtime_instances.id
 WHERE run_leases.id = $1`, work.LeaseID).Scan(
		&runtime.id, &runtime.workspaceID, &runtime.definitionID,
	); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func detachRuntimeLease(t *testing.T, fixture runtest.Fixture, work runtest.RunLease, runtimeID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	dbtest.MustExec(t, ctx, fixture.Pool, `DELETE FROM workspace_leases WHERE owner_run_lease_id = $1`, work.LeaseID)
	dbtest.MustExec(t, ctx, fixture.Pool, `UPDATE runs SET current_run_lease_id = NULL WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, ctx, fixture.Pool, `DELETE FROM run_leases WHERE id = $1`, work.LeaseID)
	dbtest.MustExec(t, ctx, fixture.Pool, `DELETE FROM workspace_mounts WHERE runtime_instance_id = $1`, runtimeID)
}

func resetRuntimeAllocated(t *testing.T, fixture runtest.Fixture, runtimeID uuid.UUID) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'allocated', observed_version = 0, observed_desired_version = 0,
       ready_at = NULL, runtime_substrate_id = NULL
 WHERE id = $1`, runtimeID)
}

func seedRuntimeSubstrate(t *testing.T, fixture runtest.Fixture, definitionID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO runtime_substrates (
    id, org_id, project_id, environment_id, deployment_definition_id,
    substrate_digest, substrate_format, substrate_contract, substrate_size_bytes
) VALUES ($1, $2, $3, $4, $5, $6, 'squashfs', 'builder-v0', 1)`,
		id, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, definitionID,
		dbtest.Digest("runtime-lifecycle-substrate-"+id.String()))
	return id
}

func TestRuntimeInstanceStaleObservedCloseAndFail(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	queries := New(fixture.Pool)
	work := fixture.AddRunLease(t, "assigned", time.Now().UTC())
	runtime := loadRuntimeLease(t, fixture, work)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET desired_state = 'closed', desired_version = 2, desired_reason = 'test_closed'
 WHERE id = $1`, runtime.id)

	if _, err := queries.MarkRuntimeInstanceClosed(ctx, MarkRuntimeInstanceClosedParams{
		ReasonCode:              pgvalue.Text("test_closed"),
		CleanupProof:            []byte(`{"method":"host_reconciled","completed_at":"2026-08-31T00:00:00Z"}`),
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		DesiredVersion:          2,
		ExpectedObservedVersion: 99,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale close observed version error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := queries.MarkRuntimeInstanceFailed(ctx, MarkRuntimeInstanceFailedParams{
		ReasonCode:              pgvalue.Text("test_failed"),
		Error:                   []byte(`{"code":"stale"}`),
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		DesiredVersion:          2,
		ExpectedObservedVersion: 99,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale fail observed version error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := queries.MarkRuntimeInstanceClosed(ctx, MarkRuntimeInstanceClosedParams{
		ReasonCode:              pgvalue.Text("test_closed"),
		CleanupProof:            []byte(`{"method":"host_reconciled","completed_at":"2026-08-31T00:00:00Z"}`),
		ID:                      pgvalue.UUID(runtime.id),
		WorkerInstanceID:        pgvalue.UUID(fixture.WorkerID),
		WorkerEpoch:             1,
		DesiredVersion:          9,
		ExpectedObservedVersion: 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale close desired version error = %v, want pgx.ErrNoRows", err)
	}
}
