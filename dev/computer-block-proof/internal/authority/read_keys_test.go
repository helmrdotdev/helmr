package authority

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

// The owner supplies a retained source, never a caller-selected set of keys.
// A production broker must retain this exact source across provider I/O and
// revalidate its runtime/candidate authority before returning plaintext.
const sourceReadKeysSQL = `SELECT k.key_id
 FROM computer_version_roots r
 JOIN computer_object_read_keys k USING(environment_id,computer_id,digest)
 WHERE r.environment_id=$1 AND r.computer_id=$2 AND r.version_id=$3
 ORDER BY k.key_id`

func (s store) publicationKeys(ctx context.Context, p publication) ([]string, error) {
	var keys []string
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		status, err := samePublication(ctx, tx, p)
		if err != nil {
			return err
		}
		if status != "constructing" && status != "registered" {
			return errConflict
		}
		rows, err := tx.Query(ctx, `SELECT write_key_id FROM computer_publications WHERE environment_id=$1 AND computer_id=$2 AND id=$3
 UNION SELECT k.key_id FROM computer_publications p
 JOIN computer_version_roots r ON r.environment_id=p.environment_id AND r.computer_id=p.computer_id AND r.version_id=p.source_pin
 JOIN computer_object_read_keys k ON k.environment_id=r.environment_id AND k.computer_id=r.computer_id AND k.digest=r.digest
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.id=$3 ORDER BY 1`, p.Env, p.Computer, p.ID)
		if err != nil {
			return err
		}
		keys, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return keys, err
}

// Recursive discovery remains an independent correctness oracle, not the lookup
// used for admission/delivery. Compare all certified objects, including obsolete
// pages in mixed-key physical packs, not just the latest logical root.
func (f *fixture) checkReadKeySummaries() {
	f.t.Helper()
	f.assertCount(0, `WITH RECURSIVE closure(environment_id,computer_id,root,digest) AS (
 SELECT environment_id,computer_id,digest,digest FROM computer_objects WHERE certified
 UNION
 SELECT c.environment_id,c.computer_id,c.root,e.child_digest
 FROM closure c JOIN computer_object_edges e ON e.environment_id=c.environment_id AND e.computer_id=c.computer_id AND e.parent_digest=c.digest
 ), expected AS (
 SELECT DISTINCT c.environment_id,c.computer_id,c.root AS digest,k.key_id
 FROM closure c JOIN computer_object_keys k USING(environment_id,computer_id,digest)
 ), actual AS (
 SELECT k.environment_id,k.computer_id,k.digest,k.key_id FROM computer_object_read_keys k
 JOIN computer_objects o USING(environment_id,computer_id,digest) WHERE o.certified
 ) SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM actual)
 UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)) mismatch`)
}

func readKeyCases(test func(string, func(*fixture))) {
	test("summaries follow certified physical keys and failed certification is atomic", func(f *fixture) {
		p, _ := f.candidate("summary")
		data := object{"summary-data", "segment", 0, 64, []string{"1"}}
		root := object{"summary-root", "root", 2, 128, []string{"1"}}
		f.ok(f.admit(f.ctx, p, data))
		f.ok(f.admit(f.ctx, p, root))
		if err := f.certify(f.ctx, p, root, []string{data.Digest}); err == nil {
			f.t.Fatal("uncertified child accepted")
		}
		f.assertCount(0, `SELECT count(*) FROM computer_object_read_keys WHERE digest LIKE 'summary-%'`)
		f.ok(f.certify(f.ctx, p, data, nil))
		f.ok(f.certify(f.ctx, p, root, []string{data.Digest}))
		f.checkReadKeySummaries()
		f.ok(f.certify(f.ctx, p, root, []string{data.Digest}))
		f.assertCount(1, `SELECT count(*) FROM computer_object_read_keys WHERE digest='summary-root'`)
		f.ok(f.abandon(f.ctx, p))
		f.ok(f.collect(f.ctx, root.Digest, nil))
		f.ok(f.collect(f.ctx, data.Digest, nil))
		f.assertCount(0, `SELECT count(*) FROM computer_object_read_keys WHERE digest LIKE 'summary-%'`)
	})
	test("source selection excludes unrelated same Computer keys and ends with owner", func(f *fixture) {
		f.rotate("computer", "2")
		p, _ := f.candidate("key-reader")
		f.rotate("computer", "3")
		keys, err := f.publicationKeys(f.ctx, p)
		f.ok(err)
		if !slices.Equal(keys, []string{"1", "2"}) {
			f.t.Fatalf("keys=%v", keys)
		}
		// The current pointer is not admitted writer authority, nor retained source.
		if err := f.admit(f.ctx, p, object{"wrong-key", "segment", 0, 64, []string{"3"}}); !errors.Is(err, errConflict) {
			f.t.Fatalf("unrelated key admitted: %v", err)
		}
		f.ok(f.abandon(f.ctx, p))
		if _, err = f.publicationKeys(f.ctx, p); !errors.Is(err, errConflict) {
			f.t.Fatalf("abandoned owner retained delivery: %v", err)
		}
	})
	test("blocked key discovery loses to abandonment", func(f *fixture) {
		p, _ := f.candidate("blocked-reader")
		tx := f.hold(`UPDATE computer_publications SET status='abandoned',source_pin=NULL WHERE environment_id='env' AND id='blocked-reader'`)
		done := runAsync(func() error { _, err := f.publicationKeys(f.ctx, p); return err })
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		if err := <-done; !errors.Is(err, errConflict) {
			f.t.Fatalf("abandoned source delivered keys: %v", err)
		}
	})
	test("retained older generation keys do not follow newer head", func(f *fixture) {
		f.rotate("computer", "2")
		p, m := f.candidate("new-key-head")
		o := object{m.Root, "root", 2, 128, []string{"2"}}
		f.ok(f.admit(f.ctx, p, o))
		f.ok(f.certify(f.ctx, p, o, nil))
		f.ok(f.seal(f.ctx, p, m))
		latest, err := f.publish(f.ctx, p, m)
		f.ok(err)
		for version, want := range map[string]string{"version-seed": "1", latest: "2"} {
			rows, err := f.pool.Query(f.ctx, sourceReadKeysSQL, "env", "computer", version)
			f.ok(err)
			keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
			f.ok(err)
			if !slices.Equal(keys, []string{want}) {
				f.t.Fatalf("version %s got keys %v", version, keys)
			}
		}
		f.checkReadKeySummaries()
	})
	test("restored key discovery does not scan graph growth", func(f *fixture) {
		for _, count := range []int{64, 4096, 16384} {
			f.t.Logf("graph objects=%d", count)
			// Build a broad already-certified physical graph as a query-cost fixture.
			// Byte authentication is covered by the encrypted-generation cases.
			f.sql(`INSERT INTO cas_object_lifetimes(digest) SELECT 'bulk-'||i FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			f.sql(`INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT 'org','bulk-'||i,64,'application/proof' FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			f.sql(`INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,certified_at)
    SELECT 'env','computer','bulk-'||i,'org','project',64,'application/proof','segment',0,now() FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			f.sql(`INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id) SELECT 'env','computer','bulk-'||i,'1' FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			f.sql(`INSERT INTO computer_object_read_keys(environment_id,computer_id,digest,key_id) SELECT 'env','computer','bulk-'||i,'1' FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			// The seed is the retained root; make all new objects reachable to ensure the
			// oracle really would traverse the growing graph.
			f.sql(`INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank)
    SELECT 'env','computer','seed-root','bulk-'||i,1,0 FROM generate_series(1,$1::int) i ON CONFLICT DO NOTHING`, count)
			f.sql(`ANALYZE computer_objects; ANALYZE computer_object_edges; ANALYZE computer_object_read_keys; ANALYZE computer_version_roots`)
			f.checkReadKeySummaries()
			var raw []byte
			f.ok(f.pool.QueryRow(f.ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sourceReadKeysSQL, "env", "computer", "version-seed").Scan(&raw))
			var plans []map[string]any
			f.ok(json.Unmarshal(raw, &plans))
			plan := plans[0]["Plan"].(map[string]any)
			inspected := 0.0
			var visit func(map[string]any)
			visit = func(p map[string]any) {
				if relation, ok := p["Relation Name"].(string); ok {
					if relation == "computer_object_edges" || relation == "computer_object_keys" || relation == "computer_objects" {
						f.t.Fatalf("graph scan in read-key lookup: %s", relation)
					}
					rows, _ := p["Actual Rows"].(float64)
					filtered, _ := p["Rows Removed by Filter"].(float64)
					loops, _ := p["Actual Loops"].(float64)
					inspected += (rows + filtered) * loops
				}
				if children, ok := p["Plans"].([]any); ok {
					for _, c := range children {
						visit(c.(map[string]any))
					}
				}
			}
			visit(plan)
			// PostgreSQL may prefer a cheap sequential scan for a tiny table.
			// Once the graph grows, the exact-root index must bound discovery.
			if count >= 4096 && inspected > 8 {
				f.t.Fatalf("key lookup inspected %.0f rows for single-key root", inspected)
			}
			f.t.Logf("discovery rows=%.0f, shared hits=%v, execution ms=%v", inspected, plan["Shared Hit Blocks"], plans[0]["Execution Time"])
			keys, err := f.publicationKeys(f.ctx, publication{Env: "env", Computer: "computer", ID: "missing", Epoch: 1})
			if err == nil || keys != nil {
				f.t.Fatal("missing owner authorized")
			}
		}
	})
}
