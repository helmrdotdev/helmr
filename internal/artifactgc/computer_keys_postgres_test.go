package artifactgc

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestComputerKeyCollectionPreservesOwnersAndErasesUnownedMaterial(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	q := db.New(f.Pool)
	collector := &Reclaimer{pool: f.Pool, queries: q}
	orphan, current, object := uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String()
	for _, id := range []string{orphan, current, object} {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,workspace_id,'fixture',decode('01','hex') FROM runs WHERE id=$1`, work.RunID, id)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT workspace_id FROM runs WHERE id=$1)`, work.RunID, current)
	digest := dbtest.Digest("key retention object")
	if _, err := q.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.OrgID), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) SELECT environment_id,workspace_id,$2,org_id,project_id,1,'application/octet-stream','segment',0,'{}' FROM runs WHERE id=$1`, work.RunID, digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) SELECT environment_id,workspace_id,$2,$3,true FROM runs WHERE id=$1`, work.RunID, digest, object)
	if err := collector.collectComputerKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{orphan, current, object} {
		var retired, erased bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL,wrapped_key IS NULL FROM computer_data_keys WHERE id=$1`, id).Scan(&retired, &erased); err != nil {
			t.Fatal(err)
		}
		if retired != (id == orphan) || erased != retired {
			t.Fatalf("key %s: retired=%v erased=%v", id, retired, erased)
		}
	}
	// A stale discovery cannot retire a key acquired by a current writer.
	if n, err := q.RetireUnreferencedComputerKey(t.Context(), pgvalue.UUID(uuid.MustParse(current))); err != nil || n != 0 {
		t.Fatalf("stale candidate: %d %v", n, err)
	}
	if _, err := f.Pool.Exec(t.Context(), `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT workspace_id FROM runs WHERE id=$1)`, work.RunID, orphan); err == nil {
		t.Fatal("retired key was adopted")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=NULL WHERE id=(SELECT workspace_id FROM runs WHERE id=$1)`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_objects WHERE digest=$1`, digest)
	if err := collector.collectComputerKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := q.RetireUnreferencedComputerKey(context.Background(), pgvalue.UUID(uuid.MustParse(current))); err != nil || n != 0 {
		t.Fatalf("retirement replay: %d %v", n, err)
	}
}

func TestComputerKeyRetirementWaitsForConcurrentAdoption(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,workspace_id,'fixture',decode('01','hex') FROM runs WHERE id=$1`, work.RunID, key.String())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT workspace_id FROM runs WHERE id=$1)`, work.RunID, key.String()); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	collector := &Reclaimer{pool: f.Pool, queries: db.New(f.Pool)}
	go func() { result <- collector.collectComputerKeys(ctx) }()
	// Discovery cannot see the uncommitted owner. The availability FK must hold
	// retirement until adoption commits, then reject retirement as a normal race.
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%RetireUnreferencedComputerKey%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-result:
			t.Fatalf("collector did not wait for adoption: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	var available bool
	if err = f.Pool.QueryRow(ctx, `SELECT available FROM computer_data_keys WHERE id=$1`, key.String()).Scan(&available); err != nil || !available {
		t.Fatalf("concurrent owner lost key: %v", err)
	}
}
