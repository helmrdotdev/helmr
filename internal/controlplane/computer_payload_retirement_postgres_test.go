package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifactgc"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func publishRetirementSave(t *testing.T, f *execGenerationFixture, sequence int64) workerapi.ComputerSaveBeginRequest {
	t.Helper()
	request := workerapi.ComputerSaveBeginRequest{OrgID: f.OrgID.String(), WorkspaceMountID: f.mountID.String(), SaveID: uuid.NewV7().String(), Sequence: sequence}
	if _, err := f.server.beginComputerSave(t.Context(), f.worker, request); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, f.root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var inspection blockformat.ObjectInspection
	if err := json.Unmarshal(raw, &inspection); err != nil {
		t.Fatal(err)
	}
	if err := f.server.recordComputerSaveObject(t.Context(), f.worker, request, inspection, "reuse"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.publishComputerSave(t.Context(), f.worker, request, f.root); err != nil {
		t.Fatal(err)
	}
	if err := f.server.adoptComputerSave(t.Context(), f.worker, request, f.root); err != nil {
		t.Fatal(err)
	}
	return request
}

func retirementSaveFixture(t *testing.T) (*execGenerationFixture, workerapi.ComputerSaveBeginRequest, *artifactgc.Reclaimer) {
	t.Helper()
	f := newExecGenerationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='running' WHERE id=$1`, f.processID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='mounted',finalization_action=NULL,finalization_reason_code=NULL,stopped_at=NULL WHERE id=$1`, f.mountID)
	first := publishRetirementSave(t, f, 1)
	publishRetirementSave(t, f, 2)
	collector, err := artifactgc.New(f.Pool, &computerGraphReclaimStore{t: t, q: f.server.db}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return f, first, collector
}

func assertPayloadState(t *testing.T, f *execGenerationFixture, id string, retired bool) {
	t.Helper()
	var tombstone, root bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT payload_retired_at IS NOT NULL,EXISTS(SELECT 1 FROM computer_version_roots r WHERE r.version_id=v.id) FROM computer_versions v WHERE v.id=$1`, id).Scan(&tombstone, &root); err != nil {
		t.Fatal(err)
	}
	if tombstone != retired || root == retired {
		t.Fatalf("payload retired=%v root=%v want retired=%v", tombstone, root, retired)
	}
}

func TestComputerPayloadRetirementPreservesAuditAndSharedGraph(t *testing.T) {
	f, first, collector := retirementSaveFixture(t)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, true)
	// The newer saved head shares these exact objects; retiring the earlier
	// locator must not retire graph bytes still needed by that head/source.
	var available bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NULL FROM cas_blobs WHERE digest=$1`, f.root.Pack.Digest).Scan(&available); err != nil || !available {
		t.Fatalf("shared bytes retired: %v", err)
	}
	if _, err := f.server.publishComputerSave(t.Context(), f.worker, first, f.root); err != nil {
		t.Fatalf("historical receipt replay: %v", err)
	}
	raw, _ := json.Marshal(f.root)
	if err := f.server.db.CreateComputerVersionRoot(t.Context(), db.CreateComputerVersionRootParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(f.computerID), VersionID: pgvalue.UUID(uuid.MustParse(first.SaveID)), Locator: raw}); err == nil {
		t.Fatal("retired payload recreated")
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE computers SET head_version_id=$2 WHERE id=$1`, f.computerID, first.SaveID); err == nil {
		t.Fatal("retired Version admitted as head")
	}
}

func TestComputerPayloadRetirementLosesToConcurrentHeadAdoption(t *testing.T) {
	f, first, collector := retirementSaveFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `UPDATE computers SET head_version_id=$2 WHERE id=$1`, f.computerID, first.SaveID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SET CONSTRAINTS computers_head_payload_fk IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- collector.Reconcile(ctx) }()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%RetireComputerVersionPayload%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-done:
			t.Fatalf("collector did not wait for owner: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, false)
}

func TestComputerPayloadRetirementRetainsLostMountStagedResult(t *testing.T) {
	f, first, collector := retirementSaveFixture(t)
	// Simulate the staged owner boundary independently of the saved head. Losing
	// the mount does not terminate the process or its finalization authority.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='exit_requested',stdout='',stderr='',staged_version_id=$2 WHERE id=$1`, f.processID, first.SaveID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_mounts SET status='lost',lost_at=now(),terminal_at=now(),terminal_reason_code='fixture' WHERE id=$1`, f.mountID)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_processes SET status='failed',terminal_at=now(),terminal_reason_code='fixture' WHERE id=$1`, f.processID)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, true)
}

func TestComputerPayloadRetirementRetainsRecoverySource(t *testing.T) {
	f, first, collector := retirementSaveFixture(t)
	// Recovery retention is independent of later Head publication and of the
	// already-adopted Runtime source. The first save otherwise has no owner.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='recovery_required',desired_state='stopped',dirty_state='dirty_state_lost',
		recovery_id=$2,recovery_version_id=$3,recovery_reason='worker_lost',
		recovery_started_at=now() WHERE id=$1`, f.computerID, uuid.NewV7(), first.SaveID)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, false)
	if _, err := f.Pool.Exec(t.Context(), `UPDATE computer_versions SET payload_retired_at=now() WHERE id=$1`, first.SaveID); err == nil {
		t.Fatal("recovery source payload retired")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean' WHERE id=$1`, f.computerID)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, false)
	// Once reconstruction is settled, the episode remains audit metadata but
	// no longer owns the old payload. Existing live owners still retain theirs.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='active',desired_state='active',dirty_state='clean',recovery_preparation_count=1,
        next_recovery_preparation_at=now(),recovery_runtime_id=$2,recovery_completed_at=now() WHERE id=$1`, f.computerID, f.runtimeID)
	if err := collector.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPayloadState(t, f, first.SaveID, true)
}
