package artifactgc

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// SQL-only descriptors exercise ownership/retirement races, not byte verification.
func TestComputerCollectorsSerializeSharedPhysicalLifetime(t *testing.T) {
	f := runtest.New(t)
	digest := dbtest.Digest("shared orphan")
	q := db.New(f.Pool)
	if _, err := q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.OrgID), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		work := f.AddRunLease(t, "assigned", time.Now())
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at)
 SELECT environment_id,workspace_id,$2,org_id,project_id,1,'application/octet-stream','segment',0,'{}',now() FROM runs WHERE id=$1`, work.RunID, digest)
	}
	candidates, err := q.ListUnreferencedComputerObjects(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	selected := make([]db.ListUnreferencedComputerObjectsRow, 0, 2)
	for _, c := range candidates {
		if c.Digest == digest {
			selected = append(selected, c)
		}
	}
	if len(selected) != 2 {
		t.Fatalf("candidates: %d", len(selected))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = db.New(blocker).LockCollectedComputerBlob(ctx, digest); err != nil {
		t.Fatal(err)
	}
	pid := blocker.Conn().PgConn().PID()
	collector := &Reclaimer{pool: f.Pool, queries: q}
	done := make(chan error, 2)
	for _, c := range selected {
		go func() { done <- collector.collectComputerObject(ctx, c) }()
	}
	// Both logical objects must be deleted before either collector can perform
	// shared membership cleanup. Observe actual PostgreSQL lock waits.
	for {
		var blocked int
		if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>$1 AND wait_event_type='Lock' AND query LIKE '%LockCollectedComputerBlob%'`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("collector did not wait for shared cleanup: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
	var retired bool
	var objects, members int
	if err = f.Pool.QueryRow(ctx, `SELECT retired_at IS NOT NULL,(SELECT count(*) FROM computer_objects WHERE digest=$1),(SELECT count(*) FROM cas_objects WHERE digest=$1) FROM cas_blobs WHERE digest=$1`, digest).Scan(&retired, &objects, &members); err != nil {
		t.Fatal(err)
	}
	if !retired || objects != 0 || members != 0 {
		t.Fatalf("shared bytes lost their cleanup owner: retired=%v objects=%d members=%d", retired, objects, members)
	}
}

// Removing a parent releases its outgoing edges; a bounded later pass then
// discovers the children. The graph remains valid at every intermediate commit.
func TestComputerCollectionDrainsOrphanGraph(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	q := db.New(f.Pool)
	digests := []string{dbtest.Digest("segment"), dbtest.Digest("index"), dbtest.Digest("root")}
	for rank, digest := range digests {
		if _, err := q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.OrgID), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream"}); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at)
 SELECT environment_id,workspace_id,$2,org_id,project_id,1,'application/octet-stream',$3,$4,'{}',now() FROM runs WHERE id=$1`, work.RunID, digest, []string{"segment", "index", "root"}[rank], rank)
		if rank > 0 {
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank)
 SELECT environment_id,workspace_id,$2,$3,$4,$4-1 FROM runs WHERE id=$1`, work.RunID, digest, digests[rank-1], rank)
		}
	}
	collector := &Reclaimer{pool: f.Pool, queries: q}
	for pass := range 3 {
		if err := collector.collectComputerObjects(t.Context()); err != nil {
			t.Fatal(err)
		}
		var remaining, retired int
		if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM computer_objects WHERE digest=ANY($1)),(SELECT count(*) FROM cas_blobs WHERE digest=ANY($1) AND retired_at IS NOT NULL)`, digests).Scan(&remaining, &retired); err != nil {
			t.Fatal(err)
		}
		if remaining != 2-pass || retired != pass+1 {
			t.Fatalf("pass %d: remaining=%d retired=%d", pass, remaining, retired)
		}
	}
}
