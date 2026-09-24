package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// This validates production ownership and atomic key derivation. Ciphertext
// verification and the fenced publication caller are separate integration gates.
func TestComputerObjectOwnershipAndCertification(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	material, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(material.Key)
	env := pgvalue.UUID(f.EnvironmentID)
	var computerID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT workspace_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	key1 := pgvalue.UUID(uuid.MustParse(material.ID))
	key2 := pgvalue.NewUUIDv7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key2, env, computerID)
	q := db.New(f.Pool)
	object := func(label, kind string, rank int, key pgtype.UUID, uploaded bool) string {
		t.Helper()
		digest := dbtest.Digest(label)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_object_lifetimes(digest) VALUES($1)`, digest)
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,64,'application/octet-stream',$6,$7,'{}')`, env, computerID, digest, f.OrgID, f.ProjectID, kind, rank)
		if key.Valid {
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, env, computerID, digest, key)
		}
		if uploaded {
			dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,64,'application/octet-stream')`, f.OrgID, digest)
		}
		return digest
	}
	certify := func(digest string) (int64, error) {
		return q.CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: digest})
	}
	state := func(err error, code string) {
		t.Helper()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != code && !(code == "23503" && pg.Code == "23001")) {
			t.Fatalf("expected SQLSTATE %s, got %v", code, err)
		}
	}
	scalar := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := f.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	child := object("child", "segment", 0, key1, true)
	parent := object("parent", "root", 2, key2, false)
	edgeSQL := `INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,$4,2,0)`
	_, err = f.Pool.Exec(t.Context(), edgeSQL, env, computerID, parent, child)
	state(err, "23503")
	if n, err := certify(child); err != nil || n != 1 {
		t.Fatalf("certify child: %d %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, edgeSQL, env, computerID, parent, child)
	// A directly used key can also be inherited. Certification inserts no new
	// row in this case and must preserve its direct provenance.
	overlap := object("overlap", "root", 2, key1, true)
	dbtest.MustExec(t, t.Context(), f.Pool, edgeSQL, env, computerID, overlap, child)
	if n, err := certify(overlap); err != nil || n != 1 {
		t.Fatalf("overlapping key certification: %d %v", n, err)
	}
	if scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1`, overlap) != 1 ||
		scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1 AND is_direct`, overlap) != 1 {
		t.Fatal("overlap duplicated the key or lost direct provenance")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_objects WHERE digest=$1`, overlap)
	// Missing exact CAS membership rolls back both certification and derived keys.
	_, err = certify(parent)
	state(err, "23503")
	if n := scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1 AND NOT is_direct`, parent); n != 0 {
		t.Fatal("failed certification leaked summary")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,63,'application/octet-stream')`, f.OrgID, parent)
	_, err = certify(parent)
	state(err, "23503")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE cas_objects SET size_bytes=64 WHERE org_id=$1 AND digest=$2`, f.OrgID, parent)
	// Hold the exact row until a contender demonstrably waits. It must
	// recheck certified after lock acquisition, not reuse a stale snapshot.
	locker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err = db.New(locker).LockComputerObject(t.Context(), db.LockComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: parent}); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err = locker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan int64, 2)
	errs := make(chan error, 2)
	for range 1 {
		wg.Go(func() { n, e := certify(parent); outcomes <- n; errs <- e })
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for scalar(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, pid) < 1 {
		select {
		case <-deadline.C:
			t.Fatal("certifiers did not block on object lock")
		case <-tick.C:
		}
	}
	if n, err := db.New(locker).CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: parent}); err != nil || n != 1 {
		t.Fatalf("winning certification: %d %v", n, err)
	}
	if err = locker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for n := range outcomes {
		if n != 0 {
			t.Fatalf("duplicate certification after lock wait: %d", n)
		}
	}
	keys, err := q.ListComputerObjectReadKeys(t.Context(), db.ListComputerObjectReadKeysParams{EnvironmentID: env, ComputerID: computerID, Digest: parent})
	if err != nil || len(keys) != 2 {
		t.Fatalf("derived keys count=%d: %v", len(keys), err)
	}
	for _, key := range keys {
		if key.ID != key1 && key.ID != key2 {
			t.Fatal("unrelated key selected")
		}
	}
	if scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1 AND is_direct`, parent) != 1 ||
		scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1 AND NOT is_direct`, parent) != 1 {
		t.Fatal("certification lost direct/inherited provenance")
	}
	// Child edges, certified CAS membership and summaries retain exact dependencies.
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE digest=$1`, child)
	state(err, "23503")
	_, err = f.Pool.Exec(t.Context(), `DELETE FROM cas_objects WHERE digest=$1`, parent)
	state(err, "23503")
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, key2)
	state(err, "23503")
	_, err = f.Pool.Exec(t.Context(), `UPDATE cas_object_lifetimes SET retired_at=now(),next_reclaim_at=now() WHERE digest=$1`, parent)
	state(err, "23503")
	// A sibling Computer in the same environment still cannot borrow these keys.
	siblingRun := f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var sibling pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT workspace_id FROM runs WHERE id=$1`, siblingRun.RunID).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	foreign := dbtest.Digest("foreign-root")
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_object_lifetimes(digest) VALUES($1)`, foreign)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,64,'application/octet-stream','root',2,'{}')`, env, sibling, foreign, f.OrgID, f.ProjectID)
	_, err = f.Pool.Exec(t.Context(), `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, env, sibling, foreign, key2)
	state(err, "23503")
	_, err = f.Pool.Exec(t.Context(), edgeSQL, env, sibling, foreign, child)
	state(err, "23503")
	// A declaration with no direct ciphertext key cannot acquire certification.
	if n, err := q.CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: sibling, Digest: foreign}); err != nil || n != 0 {
		t.Fatalf("keyless object certified: %d %v", n, err)
	}
	// Parent-first collection releases only its summary; child ownership survives.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_objects WHERE digest=$1`, parent)
	if scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1`, parent) != 0 {
		t.Fatal("summary survived parent deletion")
	}
	if scalar(`SELECT count(*) FROM computer_object_keys WHERE digest=$1`, child) != 1 {
		t.Fatal("child summary lost with parent")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, key2)
}

func TestComputerObjectCertificationRollback(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	material, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(material.Key)
	var computerID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT workspace_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	digest := dbtest.Digest("rollback-root")
	env := pgvalue.UUID(f.EnvironmentID)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_object_lifetimes(digest) VALUES($1)`, digest)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,64,'application/octet-stream')`, f.OrgID, digest)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,64,'application/octet-stream','root',1,'{}')`, env, computerID, digest, f.OrgID, f.ProjectID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, env, computerID, digest, material.ID)
	if n, err := db.New(tx).CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: digest}); err != nil || n != 1 {
		t.Fatalf("certification: %d %v", n, err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_keys WHERE digest=$1`, digest).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback retained summary: %d %v", count, err)
	}
}
