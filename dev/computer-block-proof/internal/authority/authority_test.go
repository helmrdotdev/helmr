package authority

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	store
	ctx context.Context
	t   *testing.T
}

func newFixture(t *testing.T, db dbtest.Database, n int) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	name := fmt.Sprintf("authority_%d", n)
	if _, err := db.Pool.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(db.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 8
	config.ConnConfig.RuntimeParams["search_path"] = name
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	config.ConnConfig.RuntimeParams["deadlock_timeout"] = "50"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, err := db.Pool.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{name}.Sanitize()+` CASCADE`)
		if err != nil {
			t.Error(err)
		}
	})
	f := &fixture{store{pool}, ctx, t}
	f.sql(schema)
	f.sql(`INSERT INTO environments VALUES('env','org','project'),('env2','org','project'),('env3','org2','project2'); INSERT INTO computers VALUES('env','computer',1,NULL),('env2','computer',1,NULL),('env3','computer',1,NULL)`)
	f.seed("env")
	return f
}
func (f *fixture) ok(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) sql(q string, args ...any) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, q, args...)
	f.ok(err)
}
func (f *fixture) integer(q string, args ...any) int {
	f.t.Helper()
	var n int
	f.ok(f.pool.QueryRow(f.ctx, q, args...).Scan(&n))
	return n
}
func (f *fixture) assertCount(want int, q string, args ...any) {
	f.t.Helper()
	if got := f.integer(q, args...); got != want {
		f.t.Fatalf("count %d, want %d: %s", got, want, q)
	}
}
func (f *fixture) seed(env string) {
	p := publication{Env: env, ID: "seed", Computer: "computer", Epoch: 1}
	o := object{"seed-root", "root", 1, 128}
	m := manifest{"seed-manifest", o.Digest, 0, 4096}
	f.ok(f.begin(f.ctx, p))
	f.ok(f.admit(f.ctx, p, o))
	f.ok(f.certify(f.ctx, p, o, nil))
	f.ok(f.seal(f.ctx, p, m))
	_, err := f.publish(f.ctx, p, m)
	f.ok(err)
}
func (f *fixture) candidate(id string) (publication, manifest) {
	p := publication{Env: "env", ID: id, Computer: "computer", Epoch: 1, Source: "version-seed"}
	f.ok(f.begin(f.ctx, p))
	return p, manifest{id + "-manifest", id + "-root", 0, 4096}
}
func (f *fixture) ready(p publication, m manifest, children ...string) {
	o := object{m.Root, "root", 2, 128}
	f.ok(f.admit(f.ctx, p, o))
	f.ok(f.certify(f.ctx, p, o, children))
	f.ok(f.seal(f.ctx, p, m))
}
func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}
func requireState(t *testing.T, err error, code string) {
	t.Helper()
	if sqlState(err) != code {
		t.Fatalf("got %v, want SQLSTATE %s", err, code)
	}
}
func (f *fixture) hold(q string, args ...any) pgx.Tx {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	f.ok(err)
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	f.ok(exec(f.ctx, tx, q, args...))
	return tx
}

// Observe the competing session waiting on the specific transaction, not a sleep
// that merely assumes both goroutines overlapped.
func (f *fixture) waitBlocked(tx pgx.Tx) {
	f.t.Helper()
	var pid int
	f.ok(tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if f.integer(`SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid) > 0 {
			return
		}
		select {
		case <-deadline.C:
			f.t.Fatal("competitor did not block on held transaction")
		case <-tick.C:
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		}
	}
}
func runAsync(fn func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return done
}

func TestAuthorityPostgres(t *testing.T) {
	// This opt-in proof must not silently report success when PostgreSQL is absent.
	if os.Getenv("HELMR_SKIP_POSTGRES_TESTS") == "1" {
		t.Fatal("authority proof requires PostgreSQL; remove HELMR_SKIP_POSTGRES_TESTS")
	}
	if os.Getenv("HELMR_TEST_DATABASE_URL") == "" {
		for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
			if _, err := osexec.LookPath(name); err != nil {
				t.Fatalf("authority proof requires pinned PostgreSQL: %v", err)
			}
		}
	}
	db := dbtest.Open(t)
	var version string
	if err := db.Pool.QueryRow(t.Context(), `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Log(version)
	n := 0
	test := func(name string, fn func(*fixture)) {
		t.Run(name, func(t *testing.T) { n++; fn(newFixture(t, db, n)) })
	}
	generationCases(test)

	test("registration does not grant possession and tombstones cannot revive", func(f *fixture) {
		p, _ := f.candidate("candidate")
		o := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, o))
		f.assertCount(0, `SELECT count(*) FROM cas_objects WHERE digest='data'`)
		f.ok(f.abandon(f.ctx, p))
		f.ok(f.collect(f.ctx, "data", nil))
		q, _ := f.candidate("late")
		requireState(f.t, f.admit(f.ctx, q, o), "23503")
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='data' AND NOT available`)
		f.assertCount(0, `SELECT count(*) FROM computer_publication_objects WHERE publication_id='late'`)
	})
	test("registration wins concurrent retirement", func(f *fixture) {
		p, _ := f.candidate("candidate")
		o := object{"data", "segment", 0, 64}
		f.sql(`INSERT INTO cas_object_lifetimes(digest) VALUES('data')`)
		tx := f.hold(`SELECT 1`)
		f.ok(admit(f.ctx, tx, p, o))
		done := runAsync(func() error { return f.collect(f.ctx, "data", nil) })
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		f.ok(<-done)
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='data' AND available`)
	})
	test("retirement wins concurrent admission", func(f *fixture) {
		p, _ := f.candidate("candidate")
		f.sql(`INSERT INTO cas_object_lifetimes(digest) VALUES('data')`)
		tx := f.hold(`UPDATE cas_object_lifetimes SET retired_at=now() WHERE digest='data'`)
		done := runAsync(func() error { return f.admit(f.ctx, p, object{"data", "segment", 0, 64}) })
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		requireState(f.t, <-done, "23503")
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
	})
	test("source pin survives head replacement and history outlives payload", func(f *fixture) {
		p, m := f.candidate("next")
		f.ready(p, m)
		held, _ := f.candidate("building")
		_, err := f.publish(f.ctx, p, m)
		f.ok(err)
		_, err = f.pool.Exec(f.ctx, `DELETE FROM computer_version_roots WHERE version_id='version-seed'`)
		requireState(f.t, err, "23503")
		f.ok(f.abandon(f.ctx, held))
		f.sql(`DELETE FROM computer_version_roots WHERE version_id='version-seed'`)
		f.ok(f.collect(f.ctx, "seed-root", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_versions WHERE id='version-seed'`)
		f.assertCount(1, `SELECT count(*) FROM computer_versions WHERE id='version-next' AND parent_id='version-seed'`)
	})
	test("source release wins before construction", func(f *fixture) {
		f.sql(`UPDATE computers SET head_id=NULL WHERE environment_id='env'`)
		tx := f.hold(`DELETE FROM computer_version_roots WHERE version_id='version-seed'`)
		done := runAsync(func() error {
			return f.transaction(f.ctx, func(tx pgx.Tx) error {
				return exec(f.ctx, tx, `INSERT INTO attempts(environment_id,id,base_version) VALUES('env','reader','version-seed')`)
			})
		})
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		requireState(f.t, <-done, "23503")
	})
	test("certification is serialized complete and immutable", func(f *fixture) {
		p, m := f.candidate("candidate")
		child := object{"data", "segment", 0, 64}
		parent := object{m.Root, "root", 2, 128}
		f.ok(f.admit(f.ctx, p, child))
		f.ok(f.admit(f.ctx, p, parent))
		if err := f.certify(f.ctx, p, parent, []string{"data"}); err == nil {
			f.t.Fatal("uncertified child accepted")
		}
		f.assertCount(0, `SELECT count(*) FROM cas_objects WHERE digest=$1`, parent.Digest)
		f.ok(f.certify(f.ctx, p, child, nil))
		tx := f.hold(`SELECT * FROM computer_objects WHERE environment_id='env' AND digest=$1 FOR UPDATE`, parent.Digest)
		first := runAsync(func() error { return f.certify(f.ctx, p, parent, []string{"data"}) })
		f.waitBlocked(tx)
		second := runAsync(func() error { return f.certify(f.ctx, p, parent, []string{"data"}) })
		f.ok(tx.Commit(f.ctx))
		f.ok(<-first)
		f.ok(<-second)
		f.assertCount(1, `SELECT count(*) FROM computer_object_edges WHERE parent_digest=$1`, parent.Digest)
		if err := f.certify(f.ctx, p, parent, nil); !errors.Is(err, errConflict) {
			f.t.Fatalf("partial recertification: %v", err)
		}
		changed := parent
		changed.Size++
		if err := f.admit(f.ctx, p, changed); !errors.Is(err, errConflict) {
			f.t.Fatalf("descriptor replaced: %v", err)
		}
		f.ok(f.collect(f.ctx, "data", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
	})
	test("org descriptor conflict rolls back certification", func(f *fixture) {
		p, _ := f.candidate("candidate")
		o := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, o))
		f.sql(`INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES('org','data',65,'application/proof')`)
		if err := f.certify(f.ctx, p, o, nil); !errors.Is(err, errConflict) {
			f.t.Fatalf("conflicting possession certified: %v", err)
		}
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest='data' AND certified`)
	})
	test("abandon defeats blocked certifier", func(f *fixture) {
		p, _ := f.candidate("candidate")
		o := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, o))
		tx := f.hold(`SELECT * FROM computer_publications WHERE environment_id='env' AND id='candidate' FOR UPDATE`)
		done := runAsync(func() error { return f.certify(f.ctx, p, o, nil) })
		f.waitBlocked(tx)
		f.ok(abandon(f.ctx, tx, p))
		f.ok(tx.Commit(f.ctx))
		if err := <-done; !errors.Is(err, errConflict) {
			f.t.Fatalf("stale certifier: %v", err)
		}
		f.ok(f.collect(f.ctx, "data", nil))
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
	})
	test("head fence and receipt replay after authority advances", func(f *fixture) {
		p, m := f.candidate("winner")
		f.ready(p, m)
		old, oldm := f.candidate("stale")
		f.ready(old, oldm)
		result, err := f.publish(f.ctx, p, m)
		f.ok(err)
		if _, err = f.publish(f.ctx, old, oldm); !errors.Is(err, errConflict) {
			f.t.Fatalf("old head published: %v", err)
		}
		f.sql(`UPDATE computers SET epoch=2 WHERE environment_id='env'`)
		again, err := f.publish(f.ctx, p, m)
		f.ok(err)
		if result != again {
			f.t.Fatal("replay created a second receipt")
		}
		changed := m
		changed.Capacity *= 2
		if _, err = f.publish(f.ctx, p, changed); !errors.Is(err, errConflict) {
			f.t.Fatalf("changed replay: %v", err)
		}
		f.assertCount(1, `SELECT count(*) FROM computer_versions WHERE id='version-winner'`)
	})
	test("private checkpoint remains exact after newer head", func(f *fixture) {
		f.sql(`INSERT INTO checkpoints(environment_id,id,computer_id,epoch,capture_id,scratch,memory,runtime,metadata) VALUES('env','checkpoint','computer',1,'capture','scratch-a','ram-a','runtime-a','metadata-a')`)
		p := publication{Env: "env", ID: "private", Computer: "computer", Epoch: 1, Source: "version-seed", Checkpoint: "checkpoint", Capture: "capture"}
		m := manifest{"private-manifest", "private-root", 0, 4096}
		f.ok(f.begin(f.ctx, p))
		f.ready(p, m)
		next, nextm := f.candidate("next")
		f.ready(next, nextm)
		_, err := f.publish(f.ctx, next, nextm)
		f.ok(err)
		_, err = f.publish(f.ctx, p, m)
		f.ok(err)
		f.assertCount(1, `SELECT count(*) FROM computers WHERE environment_id='env' AND head_id='version-next'`)
		f.assertCount(1, `SELECT count(*) FROM checkpoints WHERE version_id='version-private' AND scratch='scratch-a' AND memory='ram-a' AND runtime='runtime-a' AND metadata='metadata-a'`)
		wrong := p
		wrong.Capture = "other"
		if _, err = f.publish(f.ctx, wrong, m); !errors.Is(err, errConflict) {
			f.t.Fatalf("wrong capture replay: %v", err)
		}
		f.ok(f.collect(f.ctx, "private-root", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='private-root'`)
	})
	test("supersession transfers pins without a collection gap", func(f *fixture) {
		p, _ := f.candidate("old")
		o := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, o))
		f.ok(f.certify(f.ctx, p, o, nil))
		next := p
		next.ID = "next"
		tx := f.hold(`SELECT digest FROM cas_objects WHERE digest='data' FOR UPDATE`)
		done := runAsync(func() error { return f.collect(f.ctx, "data", nil) })
		f.waitBlocked(tx)
		// The collector reaches its CAS barrier while membership stays live.
		f.ok(f.supersede(f.ctx, p, next))
		f.ok(tx.Commit(f.ctx))
		f.ok(<-done)
		f.assertCount(1, `SELECT count(*) FROM computer_publication_objects WHERE publication_id='next' AND digest='data'`)
		f.assertCount(0, `SELECT count(*) FROM computer_publication_objects WHERE publication_id='old'`)
		f.ok(f.collect(f.ctx, "data", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
	})
	test("publication transfers ownership while collector waits", func(f *fixture) {
		p, m := f.candidate("candidate")
		f.ready(p, m)
		tx := f.hold(`SELECT * FROM cas_objects WHERE digest=$1 FOR UPDATE`, m.Root)
		done := runAsync(func() error { return f.collect(f.ctx, m.Root, nil) })
		f.waitBlocked(tx)
		_, err := f.publish(f.ctx, p, m)
		f.ok(err)
		f.ok(tx.Commit(f.ctx))
		f.ok(<-done)
		f.assertCount(0, `SELECT count(*) FROM computer_publication_objects WHERE publication_id='candidate'`)
		f.assertCount(1, `SELECT count(*) FROM computer_version_roots WHERE version_id='version-candidate'`)
		f.assertCount(1, `SELECT count(*) FROM cas_objects WHERE digest=$1`, m.Root)
	})
	test("artifact insertion wins concurrent cascade collection", func(f *fixture) {
		p, _ := f.candidate("candidate")
		o := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, o))
		f.ok(f.certify(f.ctx, p, o, nil))
		f.ok(f.abandon(f.ctx, p))
		tx := f.hold(`INSERT INTO artifacts VALUES('artifact','org','data',64,'application/proof')`)
		done := runAsync(func() error { return f.collect(f.ctx, "data", nil) })
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		f.ok(<-done)
		f.assertCount(1, `SELECT count(*) FROM artifacts WHERE id='artifact'`)
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='data' AND available`)
	})
	test("scope and uncertified root constraints reject unsafe owners", func(f *fixture) {
		p, m := f.candidate("candidate")
		f.ok(f.admit(f.ctx, p, object{m.Root, "root", 2, 128}))
		f.ok(f.seal(f.ctx, p, m))
		_, err := f.publish(f.ctx, p, m)
		requireState(f.t, err, "23503")
		f.assertCount(0, `SELECT count(*) FROM computer_versions WHERE id='version-candidate'`)
		_, err = f.pool.Exec(f.ctx, `INSERT INTO computer_publication_objects VALUES('env2','candidate',$1)`, m.Root)
		requireState(f.t, err, "23503")
		f.assertCount(1, `SELECT count(*) FROM computers WHERE environment_id='env' AND head_id='version-seed'`)
	})

	test("shared checkpoint and head retain child independently", func(f *fixture) {
		f.sql(`INSERT INTO checkpoints(environment_id,id,computer_id,epoch,capture_id,scratch,memory,runtime,metadata) VALUES('env','checkpoint','computer',1,'capture','s','m','r','i')`)
		private := publication{Env: "env", ID: "private", Computer: "computer", Epoch: 1, Source: "version-seed", Checkpoint: "checkpoint", Capture: "capture"}
		pm := manifest{"private-manifest", "private-root", 0, 4096}
		f.ok(f.begin(f.ctx, private))
		head, hm := f.candidate("head")
		shared := object{"shared", "segment", 0, 64}
		for _, p := range []publication{private, head} {
			f.ok(f.admit(f.ctx, p, shared))
			f.ok(f.certify(f.ctx, p, shared, nil))
		}
		f.ready(private, pm, "shared")
		f.ready(head, hm, "shared")
		_, err := f.publish(f.ctx, head, hm)
		f.ok(err)
		_, err = f.publish(f.ctx, private, pm)
		f.ok(err)
		f.sql(`UPDATE checkpoints SET version_id=NULL; DELETE FROM computer_version_roots WHERE version_id='version-private'`)
		f.ok(f.collect(f.ctx, pm.Root, nil))
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='shared'`)
		f.assertCount(1, `SELECT count(*) FROM computer_object_edges WHERE child_digest='shared' AND parent_digest='head-root'`)
		f.sql(`UPDATE computers SET head_id=NULL WHERE environment_id='env'; DELETE FROM computer_version_roots WHERE version_id='version-head'`)
		f.ok(f.collect(f.ctx, hm.Root, nil))
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='shared' AND NOT available`)
	})

	test("conditional attempt and wait retention transfer", func(f *fixture) {
		p, m := f.candidate("next")
		f.ready(p, m)
		_, err := f.publish(f.ctx, p, m)
		f.ok(err)
		f.sql(`INSERT INTO attempts(environment_id,id,base_version) VALUES('env','attempt','version-seed'); INSERT INTO waits(environment_id,id,base_version,resume_version) VALUES('env','wait','version-seed','version-next')`)
		f.sql(`UPDATE attempts SET retry_needed=false`)
		_, err = f.pool.Exec(f.ctx, `DELETE FROM computer_version_roots WHERE version_id='version-seed'`)
		requireState(f.t, err, "23503")
		f.sql(`UPDATE attempts SET consumer_excluded=true`)
		_, err = f.pool.Exec(f.ctx, `DELETE FROM computer_version_roots WHERE version_id='version-seed'`)
		requireState(f.t, err, "23503")
		f.ok(f.transaction(f.ctx, func(tx pgx.Tx) error {
			if err := exec(f.ctx, tx, `INSERT INTO attempts(environment_id,id,base_version) VALUES('env','resumed','version-next')`); err != nil {
				return err
			}
			return exec(f.ctx, tx, `UPDATE waits SET transferred=true`)
		}))
		f.sql(`DELETE FROM computer_version_roots WHERE version_id='version-seed'; UPDATE computers SET head_id=NULL WHERE environment_id='env'`)
		_, err = f.pool.Exec(f.ctx, `DELETE FROM computer_version_roots WHERE version_id='version-next'`)
		requireState(f.t, err, "23503")
		f.assertCount(1, `SELECT count(*) FROM waits WHERE base_version='version-seed' AND retained_base IS NULL`)
	})
	test("cross scope retention and artifact cascade protection", func(f *fixture) {
		var candidates []publication
		for _, env := range []string{"env", "env2", "env3"} {
			source := ""
			if env == "env" {
				source = "version-seed"
			}
			p := publication{Env: env, ID: "candidate", Computer: "computer", Epoch: 1, Source: source}
			f.ok(f.begin(f.ctx, p))
			o := object{"shared", "segment", 0, 64}
			f.ok(f.admit(f.ctx, p, o))
			f.ok(f.certify(f.ctx, p, o, nil))
			candidates = append(candidates, p)
		}
		f.sql(`INSERT INTO artifacts VALUES('artifact','org','shared',64,'application/proof')`)
		f.ok(f.abandon(f.ctx, candidates[0]))
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(2, `SELECT count(*) FROM computer_objects WHERE digest='shared'`)
		f.ok(f.abandon(f.ctx, candidates[1]))
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(1, `SELECT count(*) FROM artifacts WHERE id='artifact'`)
		f.ok(f.abandon(f.ctx, candidates[2]))
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(1, `SELECT count(*) FROM cas_objects WHERE digest='shared'`)
		f.sql(`DELETE FROM artifacts WHERE id='artifact'`)
		f.ok(f.collect(f.ctx, "shared", nil))
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='shared' AND NOT available`)
	})
	test("rollback and terminated backend leave reclaimable graph", func(f *fixture) {
		p, m := f.candidate("candidate")
		child := object{"data", "segment", 0, 64}
		f.ok(f.admit(f.ctx, p, child))
		f.ok(f.certify(f.ctx, p, child, nil))
		f.ready(p, m, "data")
		f.ok(f.abandon(f.ctx, p))
		injected := errors.New("after graph removal")
		if err := f.collect(f.ctx, m.Root, func(pgx.Tx) error { return injected }); !errors.Is(err, injected) {
			f.t.Fatalf("injection: %v", err)
		}
		f.assertCount(1, `SELECT count(*) FROM computer_object_edges WHERE child_digest='data'`)
		err := f.collect(f.ctx, m.Root, func(tx pgx.Tx) error {
			var pid int
			if err := tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			_, err := f.pool.Exec(f.ctx, `SELECT pg_terminate_backend($1)`, pid)
			return err
		})
		if err == nil {
			f.t.Fatal("terminated transaction committed")
		}
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest=$1`, m.Root)
		f.ok(f.collect(f.ctx, "data", nil))
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
		// Permanent lifetime rows discover detached objects; retry root then leaf.
		rows, err := f.pool.Query(f.ctx, `SELECT digest FROM cas_object_lifetimes WHERE available ORDER BY digest`)
		f.ok(err)
		digests, err := pgx.CollectRows(rows, pgx.RowTo[string])
		f.ok(err)
		for pass := 0; pass < 2; pass++ {
			for _, d := range digests {
				f.ok(f.collect(f.ctx, d, nil))
			}
		}
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest IN ($1,'data')`, m.Root)
		f.assertCount(2, `SELECT count(*) FROM cas_object_lifetimes WHERE digest IN ($1,'data') AND NOT available`, m.Root)
		// A tiny object-store fixture models deletion retry after late completion. This
		// does not qualify a provider's multipart/delete protocol.
		remote := map[string]bool{m.Root: true, "data": true}
		cleanup := func() {
			rows, err := f.pool.Query(f.ctx, `SELECT digest FROM cas_object_lifetimes WHERE NOT available`)
			f.ok(err)
			dead, err := pgx.CollectRows(rows, pgx.RowTo[string])
			f.ok(err)
			for _, d := range dead {
				delete(remote, d)
			}
		}
		cleanup()
		remote[m.Root] = true
		cleanup()
		if len(remote) != 0 {
			f.t.Fatal("late upload escaped repeated cleanup")
		}
	})
	for _, exhaust := range []bool{false, true} {
		name := "collector retries actual deadlock"
		if exhaust {
			name = "collector exposes retry exhaustion without partial retirement"
		}
		test(name, func(f *fixture) {
			p, _ := f.candidate("candidate")
			o := object{"data", "segment", 0, 64}
			f.ok(f.admit(f.ctx, p, o))
			f.ok(f.certify(f.ctx, p, o, nil))
			f.ok(f.abandon(f.ctx, p))
			attempts := 0
			var blockers []<-chan error
			err := f.collect(f.ctx, "data", func(tx pgx.Tx) error {
				attempts++
				if !exhaust && attempts > 1 {
					return nil
				}
				// Collector has deleted its graph row. A competing lock-order fixture owns
				// CAS membership and requests the deleted graph row. The collector next
				// requests CAS, completing a real cycle. Its shorter deadlock timer makes
				// this operation the victim, so the actual operation owner must retry.
				blocker := f.hold(`SET LOCAL deadlock_timeout='10s'`)
				f.ok(exec(f.ctx, blocker, `SELECT * FROM cas_objects WHERE digest='data' FOR UPDATE`))
				done := runAsync(func() error {
					err := exec(f.ctx, blocker, `SELECT * FROM computer_objects WHERE digest='data' FOR UPDATE`)
					rollbackErr := blocker.Rollback(context.Background())
					if err != nil {
						return err
					}
					return rollbackErr
				})
				blockers = append(blockers, done)
				f.waitBlocked(tx)
				return nil
			})
			for _, done := range blockers {
				f.ok(<-done)
			}
			if exhaust {
				if attempts != 3 || sqlState(err) != "40P01" || !strings.Contains(err.Error(), "exhausted") {
					f.t.Fatalf("exhaustion: %d %v", attempts, err)
				}
				f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE digest='data' AND certified`)
				f.assertCount(1, `SELECT count(*) FROM cas_objects WHERE digest='data'`)
				f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='data' AND available`)
			} else {
				f.ok(err)
				if attempts != 2 {
					f.t.Fatalf("operation attempts: %d", attempts)
				}
				f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest='data'`)
				f.assertCount(0, `SELECT count(*) FROM cas_objects WHERE digest='data'`)
				f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='data' AND NOT available`)
			}
		})
	}
}
