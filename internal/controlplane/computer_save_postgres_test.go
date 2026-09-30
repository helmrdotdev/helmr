package controlplane

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func instanceSaveFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workergroup.HostPrincipal, workerapi.ComputerSaveBeginRequest) {
	t.Helper()
	f := runtest.New(t)
	run := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	request := workerapi.ComputerSaveBeginRequest{EnvironmentID: f.EnvironmentID.String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id::text,i.writer_generation FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, run.LeaseID).Scan(&request.ComputerInstanceID, &request.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	return f, run, worker, request
}

func TestComputerInstanceSaveOutlivesRunAuthority(t *testing.T) {
	f, run, worker, request := instanceSaveFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE id=$1`, run.LeaseID)
	save := func(request workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		result, err := applyComputerSave(t.Context(), tx, worker, request, computerSaveBegin, nil)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		return result, err
	}
	first, err := save(request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := save(request)
	if err != nil || first != again {
		t.Fatalf("replay=%+v first=%+v err=%v", again, first, err)
	}
	if first.ComputerInstanceID != request.ComputerInstanceID || first.WriterGeneration != request.WriterGeneration || first.PredecessorID == "" {
		t.Fatalf("admission=%+v", first)
	}
	other := request
	other.SaveID = uuid.NewV7().String()
	if _, err = save(other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same sequence changed ID=%v", err)
	}
	stale := request
	stale.WriterGeneration++
	if _, err = save(stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale writer=%v", err)
	}
	wrong := request
	wrong.EnvironmentID = uuid.NewV7().String()
	if _, err = save(wrong); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong environment=%v", err)
	}
	worker.HostClaimVersion++
	if _, err = save(request); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale host claim=%v", err)
	}
	worker.HostClaimVersion--
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
	if _, err = save(request); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired replay=%v", err)
	}
}

func TestComputerInstanceSaveRollsBackAfterWriterExpires(t *testing.T) {
	f, _, worker, request := instanceSaveFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	// Establish a pending save, then let its admitted writer deadline pass while
	// still holding all locks. The final check must reject and roll back the slot.
	_, err = applyComputerSave(t.Context(), tx, worker, request, computerSaveBegin, func(tx pgx.Tx, _ *db.Queries, i db.ComputerInstance) error {
		_, err := tx.Exec(t.Context(), `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, i.WriterExpiresAt)
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired mutation=%v", err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	var empty bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT save_sequence,save_disk_version_id IS NULL FROM computer_instances WHERE id=$1`, request.ComputerInstanceID).Scan(&sequence, &empty); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 || !empty {
		t.Fatalf("rolled back slot=%d empty=%v", sequence, empty)
	}
}

func TestComputerInstanceSavePublishesThenAdoptsSource(t *testing.T) {
	f, run, worker, request := instanceSaveFixture(t)
	execute := func(op computerSaveOperation, apply func(pgx.Tx, *db.Queries, db.ComputerInstance) error) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		_, err = applyComputerSave(t.Context(), tx, worker, request, op, apply)
		if err == nil {
			err = tx.Commit(t.Context())
		}
		return err
	}
	if err := execute(computerSaveBegin, nil); err != nil {
		t.Fatal(err)
	}
	var parent, source string
	var digest string
	var logical int64
	var locator []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT v.version_id::text,v.root_pack_digest,v.logical_bytes,v.locator FROM computer_instances i JOIN computer_disk_version_roots v ON v.version_id=i.source_disk_version_id WHERE i.id=$1`, request.ComputerInstanceID).Scan(&parent, &digest, &logical, &locator); err != nil {
		t.Fatal(err)
	}
	params, err := computerSaveReceiptParams(worker, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = execute(computerSaveWrite, func(tx pgx.Tx, q *db.Queries, i db.ComputerInstance) error {
		_, err := q.PublishComputerInstanceSave(t.Context(), db.PublishComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, DesiredVersion: i.DesiredVersion, SaveID: params.SaveID, Sequence: request.Sequence, RootPackDigest: pgvalue.Text(digest), LogicalBytes: logical, Fingerprint: dbtest.Hash(request.SaveID), Locator: locator})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var head string
	var pending bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text,i.source_disk_version_id::text,i.save_disk_version_id IS NOT NULL FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, request.ComputerInstanceID).Scan(&head, &source, &pending); err != nil {
		t.Fatal(err)
	}
	if head != request.SaveID || source != parent || !pending {
		t.Fatalf("publication head=%s source=%s pending=%v", head, source, pending)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE id=$1`, run.LeaseID)
	if err = execute(computerSaveAdopt, func(tx pgx.Tx, q *db.Queries, i db.ComputerInstance) error {
		n, err := q.AdoptComputerInstanceSave(t.Context(), db.AdoptComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, Sequence: request.Sequence, SaveID: params.SaveID})
		if err == nil && n != 1 {
			return errors.New("adoption did not update one instance")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var origin string
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.source_disk_version_id::text,i.save_disk_version_id IS NOT NULL,r.base_computer_disk_version_id::text FROM computer_instances i JOIN runs r ON r.id=$2 WHERE i.id=$1`, request.ComputerInstanceID, run.RunID).Scan(&source, &pending, &origin); err != nil {
		t.Fatal(err)
	}
	if source != request.SaveID || pending || origin != parent {
		t.Fatalf("adoption source=%s pending=%v origin=%s", source, pending, origin)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, request.ComputerInstanceID)
	receipt, err := db.New(f.Pool).GetWorkerComputerSave(t.Context(), params)
	if err != nil || receipt.ID != params.SaveID {
		t.Fatalf("historical receipt=%+v err=%v", receipt, err)
	}
	request.SaveID = uuid.NewV7().String()
	request.Sequence++
	if err = execute(computerSaveBegin, nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired writer began new save: %v", err)
	}
}
