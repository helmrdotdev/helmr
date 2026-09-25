package db

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func TestRuntimeComputerSaveSlot(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	w := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	a := startTaskCompletionWork(t, ctx, f, w)
	p := BeginRuntimeComputerSaveParams{Sequence: 1, SaveID: pgvalue.UUID(uuid.NewV7()), LeaseID: pgvalue.UUID(a.workspaceLeaseID), PredecessorID: pgvalue.UUID(a.baseWorkspaceVersionID), RuntimeInstanceID: pgvalue.UUID(a.runtimeID), WorkerEpoch: 1, DesiredVersion: 1}
	if err := f.pool.QueryRow(ctx, `SELECT worker_instance_id,worker_epoch,desired_version FROM runtime_instances WHERE id=$1`, p.RuntimeInstanceID).Scan(&p.WorkerInstanceID, &p.WorkerEpoch, &p.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	q := New(f.pool)
	type result struct {
		params BeginRuntimeComputerSaveParams
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		candidate := p
		candidate.SaveID = pgvalue.UUID(uuid.NewV7())
		go func() { _, err := q.BeginRuntimeComputerSave(ctx, candidate); results <- result{candidate, err} }()
	}
	winners := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			p = r.params
			winners++
		} else if !errors.Is(r.err, pgx.ErrNoRows) {
			t.Fatal(r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent winners: %d", winners)
	}
	first, err := q.BeginRuntimeComputerSave(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*BeginRuntimeComputerSaveParams){
		func(p *BeginRuntimeComputerSaveParams) { p.WorkerEpoch++ },
		func(p *BeginRuntimeComputerSaveParams) { p.DesiredVersion++ },
		func(p *BeginRuntimeComputerSaveParams) { p.LeaseID = pgvalue.UUID(uuid.NewV7()) },
		func(p *BeginRuntimeComputerSaveParams) { p.PredecessorID = pgvalue.UUID(uuid.NewV7()) },
	} {
		bad := p
		mutation(&bad)
		if _, err := q.BeginRuntimeComputerSave(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("changed authority accepted: %v", err)
		}
	}
	replay, err := q.BeginRuntimeComputerSave(ctx, p)
	if err != nil || first != replay {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	other := p
	other.SaveID = pgvalue.UUID(uuid.NewV7())
	if _, err := q.BeginRuntimeComputerSave(ctx, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("competing operation: %v", err)
	}
	other.Sequence++
	if _, err := q.BeginRuntimeComputerSave(ctx, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping operation: %v", err)
	}
	clear := AbandonRuntimeComputerSaveParams{RuntimeInstanceID: p.RuntimeInstanceID, WorkerInstanceID: p.WorkerInstanceID, WorkerEpoch: p.WorkerEpoch, Sequence: p.Sequence, SaveID: p.SaveID, LeaseID: p.LeaseID}
	stale := clear
	stale.SaveID = other.SaveID
	if n, err := q.AbandonRuntimeComputerSave(ctx, stale); err != nil || n != 0 {
		t.Fatalf("wrong clear: %d %v", n, err)
	}
	if n, err := q.AbandonRuntimeComputerSave(ctx, clear); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if _, err := q.BeginRuntimeComputerSave(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resurrected old operation: %v", err)
	}
	if _, err := q.BeginRuntimeComputerSave(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonRuntimeComputerSave(ctx, clear); err != nil || n != 0 {
		t.Fatalf("late clear: %d %v", n, err)
	}
}

func TestReclaimedComputerSaveCleanupRequiresPhysicalExclusion(t *testing.T) {
	ctx := t.Context()
	f := newRunLeaseClaimFixture(t, ctx)
	w := f.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	a := startTaskCompletionWork(t, ctx, f, w)
	q := New(f.pool)
	p := BeginRuntimeComputerSaveParams{Sequence: 1, SaveID: pgvalue.UUID(uuid.NewV7()), LeaseID: pgvalue.UUID(a.workspaceLeaseID), PredecessorID: pgvalue.UUID(a.baseWorkspaceVersionID), RuntimeInstanceID: pgvalue.UUID(a.runtimeID)}
	if err := f.pool.QueryRow(ctx, `SELECT worker_instance_id,worker_epoch,desired_version FROM runtime_instances WHERE id=$1`, p.RuntimeInstanceID).Scan(&p.WorkerInstanceID, &p.WorkerEpoch, &p.DesiredVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := q.BeginRuntimeComputerSave(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := pgvalue.UUID(uuid.NewV7())
	if _, err := f.pool.Exec(ctx, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,workspace_id,'fixture',decode('01','hex') FROM runtime_instances WHERE id=$1`, p.RuntimeInstanceID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runtime_instances SET computer_write_key_id=$2 WHERE id=$1`, p.RuntimeInstanceID, key); err != nil {
		t.Fatal(err)
	}
	if n, err := q.RetireUnreferencedComputerKey(ctx, key); err != nil || n != 0 {
		t.Fatalf("live Runtime key retired: %d %v", n, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1,preparation_expires_at=now()-interval '1 hour' WHERE id=$1`, p.RuntimeInstanceID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 0 {
		t.Fatalf("released before physical reclaim: %d %v", n, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runtime_instances SET observed_state='closed',terminal_at=now(),terminal_reason_code='fixture',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_process_id=NULL,reserved_workspace_version_id=NULL,reservation_expires_at=NULL,reclaimed_at=now(),reclaim_evidence='{"fixture":"physical exclusion"}' WHERE id=$1`, p.RuntimeInstanceID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if n, err := q.RetireUnreferencedComputerKey(ctx, key); err != nil || n != 1 {
		t.Fatalf("reclaimed Runtime key: %d %v", n, err)
	}
	var sequence int64
	var cleared bool
	if err := f.pool.QueryRow(ctx, `SELECT computer_save_sequence,computer_save_id IS NULL AND computer_save_lease_id IS NULL AND computer_save_predecessor_id IS NULL FROM runtime_instances WHERE id=$1`, p.RuntimeInstanceID).Scan(&sequence, &cleared); err != nil {
		t.Fatal(err)
	}
	if sequence != 1 || !cleared {
		t.Fatalf("lost watermark or incomplete release: %d %v", sequence, cleared)
	}
	if _, err := q.BeginRuntimeComputerSave(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaimed producer admitted: %v", err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
}
