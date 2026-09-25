package db_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifactgc"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func requireFK(t *testing.T, err error) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatalf("want FK conflict, got %v", err)
	}
}

// Use a current checkpoint upload owner to exercise physical CAS retirement.
// Computer generations use their own immutable object graph and retention pins.
type checkpointUploadCandidate struct {
	ID        uuid.UUID
	Digest    string
	SizeBytes int64
	MediaType string
}

func checkpointUpload(t *testing.T, f runtest.Fixture) checkpointUploadCandidate {
	t.Helper()
	work := f.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	p := checkpointUploadCandidate{ID: uuid.NewV7(), Digest: dbtest.Digest(uuid.NewV7().String()), SizeBytes: 1024, MediaType: "application/octet-stream"}
	waitID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_waits(id,environment_id,run_id,workspace_id,kind,due_at,expected_run_revision,attempt_number,current_run_lease_id,resume_attach_id)
 SELECT $1,environment_id,id,workspace_id,'timer',now()+interval '1 minute',revision,1,$2,$3 FROM runs WHERE id=$4`, waitID, work.LeaseID, uuid.NewV7(), work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_checkpoints(id,run_id,attempt_number,run_wait_id,source_run_lease_id,source_workspace_lease_id,workspace_id,base_workspace_version_id)
 SELECT $1,$2,1,$3,$4,id,workspace_id,base_workspace_version_id FROM workspace_leases WHERE owner_run_lease_id=$4`, p.ID, work.RunID, waitID, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2)`, p.Digest, p.SizeBytes)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_checkpoint_objects(checkpoint_id,role,digest,size_bytes,media_type,checkpoint_status) VALUES($1,'memory',$2,$3,$4,'creating')`, p.ID, p.Digest, p.SizeBytes, p.MediaType)
	return p
}
func abandonCheckpointUpload(t *testing.T, f runtest.Fixture, p checkpointUploadCandidate) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_checkpoints SET status='invalid',invalidated_at=now(),invalidation_reason_code='checkpoint_failed' WHERE id=$1`, p.ID)
}

func TestCasRetirementPinsAndCrossOrganizationAdoption(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := checkpointUpload(t, f)
	// A direct physical-key mutation cannot bypass a registered upload pin.
	_, err := f.Pool.Exec(t.Context(), `UPDATE cas_blobs SET retired_at=now(),next_reclaim_at=now() WHERE digest=$1`, p.Digest)
	requireFK(t, err)
	abandonCheckpointUpload(t, f, p)
	// Even another organization's observed membership protects the global key.
	otherOrg := pgvalue.UUID(uuid.NewV7())
	// One physical digest cannot acquire a conflicting size in another organization.
	_, err = q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: otherOrg, Digest: p.Digest, SizeBytes: p.SizeBytes + 1, MediaType: p.MediaType})
	requireFK(t, err)
	if _, err := q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: otherOrg, Digest: p.Digest, SizeBytes: p.SizeBytes, MediaType: p.MediaType}); err != nil {
		t.Fatal(err)
	}
	_, err = q.RetireAbandonedCasBlob(t.Context(), p.Digest)
	requireFK(t, err)
	// Removing this test-only membership makes the abandoned object collectible.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM cas_objects WHERE org_id=$1 AND digest=$2`, otherOrg, p.Digest)
	if n, err := q.RetireAbandonedCasBlob(t.Context(), p.Digest); err != nil || n != 1 {
		t.Fatalf("retire: %d %v", n, err)
	}
	_, err = q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.OrgID), Digest: p.Digest, SizeBytes: p.SizeBytes, MediaType: p.MediaType})
	requireFK(t, err)
	// A future direct SQL adopter is subject to the same guard.
	_, err = f.Pool.Exec(t.Context(), `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,$4)`, otherOrg, p.Digest, p.SizeBytes, p.MediaType)
	requireFK(t, err)
	// Same-owner replay cannot reopen the upload pin after abandonment.
	_, err = f.Pool.Exec(t.Context(), `UPDATE run_checkpoints SET status='creating',invalidated_at=NULL,invalidation_reason_code=NULL WHERE id=$1`, p.ID)
	requireFK(t, err)
}

func TestCasRetirementAdoptionRaces(t *testing.T) {
	for _, retireFirst := range []bool{true, false} {
		name := "adoption-first"
		if retireFirst {
			name = "retirement-first"
		}
		t.Run(name, func(t *testing.T) {
			f := runtest.New(t)
			p := checkpointUpload(t, f)
			abandonCheckpointUpload(t, f, p)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			first, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(context.Background())
			adopt := func(q *db.Queries) error {
				_, err := q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(uuid.NewV7()), Digest: p.Digest, SizeBytes: p.SizeBytes, MediaType: p.MediaType})
				return err
			}
			retire := func(q *db.Queries) error { _, err := q.RetireAbandonedCasBlob(ctx, p.Digest); return err }
			firstOp, secondOp := adopt, retire
			if retireFirst {
				firstOp, secondOp = retire, adopt
			}
			if err := firstOp(db.New(first)); err != nil {
				t.Fatal(err)
			}
			conn, err := f.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			pid := conn.Conn().PgConn().PID()
			result := make(chan error, 1)
			go func() { result <- secondOp(db.New(conn)) }()
			// Observe an actual database wait before releasing the winner. This
			// exercises a statement whose snapshot predates the winner's commit.
			for {
				var blocked bool
				if err := f.Pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("did not block: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			requireFK(t, <-result)
		})
	}
}

type reclaimProbe struct {
	t          *testing.T
	q          *db.Queries
	uploads    []string
	versions   int
	aborts     int
	failDelete bool
	slowID     string
	attempted  map[string]bool
}

func (s *reclaimProbe) RetiredUploads(context.Context, string) ([]string, error) {
	return s.uploads, nil
}
func (s *reclaimProbe) ReclaimUpload(ctx context.Context, digest, id string) error {
	retained, err := s.q.ListRetiredCasUploads(ctx, digest)
	if err != nil {
		return err
	}
	found := false
	for _, v := range retained {
		if v == id {
			found = true
		}
	}
	if !found {
		s.t.Fatal("abort before durable upload ownership")
	}
	s.aborts++
	if s.attempted != nil {
		s.attempted[id] = true
	}
	if id == s.slowID {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (s *reclaimProbe) ReclaimVersions(ctx context.Context, digest string) error {
	if ctx.Err() != nil {
		s.t.Fatal("multipart work exhausted version deadline")
	}
	// A storage callback is after retirement commit, including on another DB
	// connection. Attempting adoption must fail before any destructive action.
	_, err := s.q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(uuid.NewV7()), Digest: digest, SizeBytes: 1, MediaType: "test"})
	requireFK(s.t, err)
	if s.failDelete {
		return errors.New("remote delete failed")
	}
	s.versions = 0
	return nil
}
func TestCasReclamationRetainsLateUploadsAndFailures(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := checkpointUpload(t, f)
	abandonCheckpointUpload(t, f, p)
	store := &reclaimProbe{t: t, q: q, uploads: []string{"upload-1"}, versions: 1, failDelete: true}
	r, err := artifactgc.New(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("lost remote failure")
	}
	var recorded string
	if err := f.Pool.QueryRow(t.Context(), `SELECT last_reclaim_error FROM cas_blobs WHERE digest=$1 AND retired_at IS NOT NULL`, p.Digest).Scan(&recorded); err != nil || recorded == "" {
		t.Fatalf("lost retry duty: %q %v", recorded, err)
	}
	store.failDelete = false
	for range 2 {
		// Includes a late PUT after a successful empty pass. An upload that no
		// longer appears in discovery still has its retained ID retried.
		store.uploads = nil
		store.versions = 1
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_upload_reclaims SET next_reclaim_at=now() WHERE digest=$1`, p.Digest)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_blobs SET next_reclaim_at=now() WHERE digest=$1`, p.Digest)
		if err := r.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if store.versions != 0 {
			t.Fatal("late version leaked")
		}
	}
	if store.aborts != 3 {
		t.Fatalf("forgot multipart ID: %d aborts", store.aborts)
	}
	// Recreating the process after a claim/connection loss needs no local state.
	r, err = artifactgc.New(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_upload_reclaims SET next_reclaim_at=now() WHERE digest=$1`, p.Digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_blobs SET next_reclaim_at=now() WHERE digest=$1`, p.Digest)
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.aborts != 4 {
		t.Fatal("restart forgot durable duty")
	}
}

func TestCasReclamationFairMultipartProgress(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := checkpointUpload(t, f)
	abandonCheckpointUpload(t, f, p)
	store := &reclaimProbe{t: t, q: q, slowID: "00", attempted: make(map[string]bool), versions: 1}
	for i := range 25 {
		store.uploads = append(store.uploads, fmt.Sprintf("%02d", i))
	}
	r, err := artifactgc.New(f.Pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// One slow old upload exhausts its own deadline. It cannot spend the budget
	// of later uploads or of completed object version deletion.
	if err := r.Reconcile(t.Context()); err == nil {
		t.Fatal("lost timed-out upload")
	}
	if store.aborts != 10 || store.versions != 0 {
		t.Fatalf("unbounded or starved pass: %d %d", store.aborts, store.versions)
	}
	store.uploads = nil
	for range 2 {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_blobs SET next_reclaim_at=now() WHERE digest=$1`, p.Digest)
		if err := r.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.attempted) != 25 || store.aborts != 25 {
		t.Fatalf("old IDs starved later ones: %d %d", len(store.attempted), store.aborts)
	}
}
