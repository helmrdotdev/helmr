package computer

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// A committed save's publication fingerprint is persisted: the encoding of
// the save and its root must stay byte-identical.
func TestSaveFingerprintIsStable(t *testing.T) {
	ref := SaveRef{
		EnvironmentID: uuid.MustParse("01997f91-0564-7000-a000-000000000003"), InstanceID: uuid.MustParse("01997f91-0564-7000-a000-000000000004"),
		WriterGeneration: 3, SaveID: uuid.MustParse("01997f91-0564-7000-a000-000000000005"), Sequence: 7,
	}
	root := disk.VersionRoot{FormatVersion: 1, LogicalBytes: 1 << 30,
		Pack: disk.VersionPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 1024, Rank: 2},
		Page: disk.VersionPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: "01912345-6789-7abc-8def-0123456789ab", Kind: 3, Count: 1, SizeBytes: 128}, Offset: 8}
	fingerprint, err := saveFingerprint(ref, root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(fingerprint[:]), "3b43d837ba9bc0a3d21f6b18bdc1197b4a3d15e324a177a9d4905a446c3c2041"; got != want {
		t.Fatalf("save fingerprint = %s, want %s", got, want)
	}
}

func instanceSaveFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workergroup.HostPrincipal, SaveRef) {
	t.Helper()
	f := runtest.New(t)
	run := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	ref := SaveRef{EnvironmentID: f.EnvironmentID, SaveID: uuid.NewV7(), Sequence: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, run.LeaseID).Scan(&ref.InstanceID, &ref.WriterGeneration); err != nil {
		t.Fatal(err)
	}
	worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	return f, run, worker, ref
}

// inSave runs fn under a save authority in one transaction and commits when
// the writer recheck still holds.
func inSave[A interface{ recheckWriter(context.Context) error }](t *testing.T, f runtest.Fixture, lock func(context.Context, pgx.Tx) (A, error), fn func(pgx.Tx, A) error) error {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	a, err := lock(t.Context(), tx)
	if err != nil {
		return err
	}
	if fn != nil {
		if err = fn(tx, a); err != nil {
			return err
		}
	}
	if err = a.recheckWriter(t.Context()); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func beginSave(t *testing.T, f runtest.Fixture, worker workergroup.HostPrincipal, ref SaveRef) (saveBegin, error) {
	t.Helper()
	var begun saveBegin
	err := inSave(t, f, func(ctx context.Context, tx pgx.Tx) (saveBegin, error) { return lockSaveBegin(ctx, tx, worker, ref) }, func(_ pgx.Tx, s saveBegin) error {
		begun = s
		return nil
	})
	return begun, err
}

func TestComputerInstanceSaveOutlivesRunAuthority(t *testing.T) {
	f, run, worker, ref := instanceSaveFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE id=$1`, run.LeaseID)
	first, err := beginSave(t, f, worker, ref)
	if err != nil {
		t.Fatal(err)
	}
	again, err := beginSave(t, f, worker, ref)
	if err != nil || first.predecessor != again.predecessor || first.instance.DesiredVersion != again.instance.DesiredVersion {
		t.Fatalf("replay=%+v first=%+v err=%v", again.predecessor, first.predecessor, err)
	}
	if first.instance.ID != pgvalue.UUID(ref.InstanceID) || first.instance.WriterGeneration != ref.WriterGeneration || !first.predecessor.Valid {
		t.Fatalf("admission=%+v", first.instance)
	}
	other := ref
	other.SaveID = uuid.NewV7()
	if _, err = beginSave(t, f, worker, other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same sequence changed ID=%v", err)
	}
	stale := ref
	stale.WriterGeneration++
	if _, err = beginSave(t, f, worker, stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale writer=%v", err)
	}
	wrong := ref
	wrong.EnvironmentID = uuid.NewV7()
	if _, err = beginSave(t, f, worker, wrong); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong environment=%v", err)
	}
	worker.HostClaimVersion++
	if _, err = beginSave(t, f, worker, ref); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale host claim=%v", err)
	}
	worker.HostClaimVersion--
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, pgvalue.UUID(ref.InstanceID))
	if _, err = beginSave(t, f, worker, ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired replay=%v", err)
	}
}

func TestComputerInstanceSaveRollsBackAfterWriterExpires(t *testing.T) {
	f, _, worker, ref := instanceSaveFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, pgvalue.UUID(ref.InstanceID))
	// Establish a pending save, then let its admitted writer deadline pass while
	// still holding all locks. The final check must reject and roll back the slot.
	err := inSave(t, f, func(ctx context.Context, tx pgx.Tx) (saveBegin, error) { return lockSaveBegin(ctx, tx, worker, ref) }, func(tx pgx.Tx, s saveBegin) error {
		_, err := tx.Exec(t.Context(), `SELECT pg_sleep(GREATEST(0,EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp())))+0.01)`, s.instance.WriterExpiresAt)
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired mutation=%v", err)
	}
	var sequence int64
	var empty bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT save_sequence,save_disk_version_id IS NULL FROM computer_instances WHERE id=$1`, pgvalue.UUID(ref.InstanceID)).Scan(&sequence, &empty); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 || !empty {
		t.Fatalf("rolled back slot=%d empty=%v", sequence, empty)
	}
}

func TestComputerInstanceSavePublishesThenAdoptsSource(t *testing.T) {
	f, run, worker, ref := instanceSaveFixture(t)
	if _, err := beginSave(t, f, worker, ref); err != nil {
		t.Fatal(err)
	}
	instanceID := pgvalue.UUID(ref.InstanceID)
	var parent, source string
	var digest string
	var logical int64
	var locator []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT v.version_id::text,v.root_pack_digest,v.logical_bytes,v.locator FROM computer_instances i JOIN computer_disk_version_roots v ON v.version_id=i.source_disk_version_id WHERE i.id=$1`, instanceID).Scan(&parent, &digest, &logical, &locator); err != nil {
		t.Fatal(err)
	}
	if err := inSave(t, f, func(ctx context.Context, tx pgx.Tx) (unpublishedSave, error) {
		return lockUnpublishedSave(ctx, tx, worker, ref)
	}, func(tx pgx.Tx, s unpublishedSave) error {
		i := s.instance
		_, err := db.New(tx).PublishComputerInstanceSave(t.Context(), db.PublishComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, DesiredVersion: i.DesiredVersion, SaveID: pgvalue.UUID(ref.SaveID), Sequence: ref.Sequence, RootPackDigest: pgvalue.Text(digest), LogicalBytes: logical, Fingerprint: dbtest.Hash(ref.SaveID.String()), Locator: locator})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var head string
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id::text,i.source_disk_version_id::text,i.save_disk_version_id IS NOT NULL FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, instanceID).Scan(&head, &source, &pending); err != nil {
		t.Fatal(err)
	}
	if head != ref.SaveID.String() || source != parent || !pending {
		t.Fatalf("publication head=%s source=%s pending=%v", head, source, pending)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=now()-interval '2 seconds',expires_at=now()-interval '1 second' WHERE id=$1`, run.LeaseID)
	if err := inSave(t, f, func(ctx context.Context, tx pgx.Tx) (publishedSave, error) {
		return lockPublishedSave(ctx, tx, worker, ref)
	}, func(tx pgx.Tx, s publishedSave) error {
		i := s.instance
		n, err := db.New(tx).AdoptComputerInstanceSave(t.Context(), db.AdoptComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, Sequence: ref.Sequence, SaveID: pgvalue.UUID(ref.SaveID)})
		if err == nil && n != 1 {
			return errors.New("adoption did not update one instance")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var origin string
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.source_disk_version_id::text,i.save_disk_version_id IS NOT NULL,r.base_computer_disk_version_id::text FROM computer_instances i JOIN runs r ON r.id=$2 WHERE i.id=$1`, instanceID, run.RunID).Scan(&source, &pending, &origin); err != nil {
		t.Fatal(err)
	}
	if source != ref.SaveID.String() || pending || origin != parent {
		t.Fatalf("adoption source=%s pending=%v origin=%s", source, pending, origin)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, instanceID)
	receipt, err := db.New(f.Pool).GetWorkerComputerSave(t.Context(), ref.receipt(worker))
	if err != nil || receipt.ID != pgvalue.UUID(ref.SaveID) {
		t.Fatalf("historical receipt=%+v err=%v", receipt, err)
	}
	ref.SaveID = uuid.NewV7()
	ref.Sequence++
	if _, err = beginSave(t, f, worker, ref); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired writer began new save: %v", err)
	}
}

func TestComputerSaveContinuesAcrossMemberWait(t *testing.T) {
	for _, admission := range []string{"open", "draining"} {
		t.Run(admission, func(t *testing.T) {
			f, member, worker, ref := instanceSaveFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, pgvalue.UUID(ref.InstanceID), admission)
			unpublished := func(ctx context.Context, tx pgx.Tx) (unpublishedSave, error) {
				return lockUnpublishedSave(ctx, tx, worker, ref)
			}
			if _, err := beginSave(t, f, worker, ref); err != nil {
				t.Fatal(err)
			}
			// Member waiting does not freeze the shared physical writer.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting',active_started_at=NULL WHERE id=$1`, member.RunID)
			if _, err := beginSave(t, f, worker, ref); err != nil {
				t.Fatalf("pending replay during wait: %v", err)
			}
			if err := inSave(t, f, unpublished, nil); err != nil {
				t.Fatalf("pending write during wait: %v", err)
			}
			if err := inSave(t, f, unpublished, func(tx pgx.Tx, s unpublishedSave) error {
				i := s.instance
				n, err := db.New(tx).AbandonComputerInstanceSave(t.Context(), db.AbandonComputerInstanceSaveParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, Sequence: ref.Sequence, SaveID: pgvalue.UUID(ref.SaveID)})
				if err == nil && n != 1 {
					return errors.New("save slot was not released")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ref.SaveID = uuid.NewV7()
			ref.Sequence++
			if _, err := beginSave(t, f, worker, ref); err != nil {
				t.Fatalf("next save during wait: %v", err)
			}
		})
	}
}

func TestComputerSaveRejectsSealedInstance(t *testing.T) {
	for _, admission := range []string{"checkpointing", "closed"} {
		t.Run(admission, func(t *testing.T) {
			f, _, worker, ref := instanceSaveFixture(t)
			// Even after the host observes the desired version, capture admission
			// cannot reopen live disk publication from a frozen Computer.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE id=$1`, pgvalue.UUID(ref.InstanceID), admission)
			if _, err := beginSave(t, f, worker, ref); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("sealed writer save: %v", err)
			}
		})
	}
}
