package db_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestComputerInstanceSaveSlot(t *testing.T) {
	ctx := t.Context()
	f, p := instanceSaveSlotFixture(t)
	q := db.New(f.Pool)
	type result struct {
		params db.BeginComputerInstanceSaveParams
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		candidate := p
		candidate.SaveID = pgvalue.UUID(uuid.NewV7())
		go func() { _, err := q.BeginComputerInstanceSave(ctx, candidate); results <- result{candidate, err} }()
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
	first, err := q.BeginComputerInstanceSave(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*db.BeginComputerInstanceSaveParams){
		func(p *db.BeginComputerInstanceSaveParams) { p.WorkerEpoch++ },
		func(p *db.BeginComputerInstanceSaveParams) { p.DesiredVersion++ },
		func(p *db.BeginComputerInstanceSaveParams) { p.WriterGeneration++ },
		func(p *db.BeginComputerInstanceSaveParams) { p.PredecessorID = pgvalue.UUID(uuid.NewV7()) },
	} {
		bad := p
		mutation(&bad)
		if _, err := q.BeginComputerInstanceSave(ctx, bad); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("changed authority accepted: %v", err)
		}
	}
	replay, err := q.BeginComputerInstanceSave(ctx, p)
	if err != nil || first != replay {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	other := p
	other.SaveID = pgvalue.UUID(uuid.NewV7())
	if _, err := q.BeginComputerInstanceSave(ctx, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("competing operation: %v", err)
	}
	other.Sequence++
	if _, err := q.BeginComputerInstanceSave(ctx, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("overlapping operation: %v", err)
	}
	clear := db.AbandonComputerInstanceSaveParams{EnvironmentID: p.EnvironmentID, WriterGeneration: p.WriterGeneration, WriterTokenHash: p.WriterTokenHash, ComputerInstanceID: p.ComputerInstanceID, WorkerHostID: p.WorkerHostID, WorkerEpoch: p.WorkerEpoch, Sequence: p.Sequence, SaveID: p.SaveID}
	stale := clear
	stale.SaveID = other.SaveID
	if n, err := q.AbandonComputerInstanceSave(ctx, stale); err != nil || n != 0 {
		t.Fatalf("wrong clear: %d %v", n, err)
	}
	if n, err := q.AbandonComputerInstanceSave(ctx, clear); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if _, err := q.BeginComputerInstanceSave(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resurrected old operation: %v", err)
	}
	if _, err := q.BeginComputerInstanceSave(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonComputerInstanceSave(ctx, clear); err != nil || n != 0 {
		t.Fatalf("late clear: %d %v", n, err)
	}
}

func TestReclaimedComputerSaveCleanupRequiresPhysicalExclusion(t *testing.T) {
	ctx := t.Context()
	f, p := instanceSaveSlotFixture(t)
	q := db.New(f.Pool)
	if _, err := q.BeginComputerInstanceSave(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := pgvalue.UUID(uuid.NewV7())
	if _, err := f.Pool.Exec(ctx, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,computer_id,'fixture',decode('01','hex') FROM computer_instances WHERE id=$1`, p.ComputerInstanceID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(ctx, `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, p.ComputerInstanceID, key); err != nil {
		t.Fatal(err)
	}
	if n, err := q.RetireUnreferencedComputerKey(ctx, key); err != nil || n != 0 {
		t.Fatalf("live Instance key retired: %d %v", n, err)
	}
	if _, err := f.Pool.Exec(ctx, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,preparation_expires_at=now()-interval '1 hour' WHERE id=$1`, p.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 0 {
		t.Fatalf("released before physical reclaim: %d %v", n, err)
	}
	if _, err := f.Pool.Exec(ctx, `UPDATE computer_instances SET observed_state='closed',terminal_at=now(),terminal_reason_code='fixture',mount_state='unmounted',unmounted_at=now(),admission_state='closed',reclaimed_at=now(),reclaim_evidence='{"fixture":"physical exclusion"}' WHERE id=$1`, p.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if n, err := q.RetireUnreferencedComputerKey(ctx, key); err != nil || n != 1 {
		t.Fatalf("reclaimed Instance key: %d %v", n, err)
	}
	var sequence int64
	var cleared bool
	if err := f.Pool.QueryRow(ctx, `SELECT save_sequence,save_disk_version_id IS NULL AND save_base_disk_version_id IS NULL FROM computer_instances WHERE id=$1`, p.ComputerInstanceID).Scan(&sequence, &cleared); err != nil {
		t.Fatal(err)
	}
	if sequence != 1 || !cleared {
		t.Fatalf("lost watermark or incomplete release: %d %v", sequence, cleared)
	}
	if _, err := q.BeginComputerInstanceSave(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reclaimed producer admitted: %v", err)
	}
	if n, err := q.AbandonReclaimedComputerSaves(ctx, 100); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
}

func instanceSaveSlotFixture(t *testing.T) (runtest.Fixture, db.BeginComputerInstanceSaveParams) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	p := db.BeginComputerInstanceSaveParams{Sequence: 1, SaveID: pgvalue.UUID(uuid.NewV7())}
	err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.environment_id,i.worker_host_id,i.worker_epoch,i.desired_version,i.writer_generation,i.writer_token_hash,c.head_disk_version_id
 FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id JOIN computers c ON c.id=i.computer_id WHERE l.id=$1`, work.LeaseID).Scan(&p.ComputerInstanceID, &p.EnvironmentID, &p.WorkerHostID, &p.WorkerEpoch, &p.DesiredVersion, &p.WriterGeneration, &p.WriterTokenHash, &p.PredecessorID)
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}
