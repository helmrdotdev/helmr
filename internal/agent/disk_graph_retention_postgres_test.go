package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDiskGraphRootAdoptionArbitratesDeletion(t *testing.T) {
	for _, adoptFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(adoptFirst), func(t *testing.T) {
			f := newFixture(t)
			var root uuid.UUID
			var digest []byte
			if err := f.pool.QueryRow(t.Context(), `SELECT initial_root_id,initial_root_digest FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer).Scan(&root, &digest); err != nil {
				t.Fatal(err)
			}
			// Replace the seed through the real storage fixture, leaving its original
			// descriptor eligible for collection. Both descriptors contain valid roots.
			newSaveStorageFixture(t, f)
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			adopt := func(tx pgx.Tx) error {
				_, err := tx.Exec(t.Context(), `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) VALUES($1,$2,$3,$4)`, f.env, uuid.NewV7(), root, digest)
				return err
			}
			if adoptFirst {
				if err := adopt(tx); err != nil {
					t.Fatal(err)
				}
				// The stale candidate's DELETE meets the still-uncommitted attachment's FK.
				if err := reclaimComputerGraphRow(t.Context(), f.pool, `DELETE FROM computer_disk_roots WHERE id=$1`, root); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := collectComputerDiskGraph(t.Context(), f.pool); err != nil {
					t.Fatal(err)
				}
				var present bool
				if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_disk_roots WHERE id=$1)`, root).Scan(&present); err != nil || !present {
					t.Fatalf("adopted root collected: %v %v", present, err)
				}
			} else {
				if _, err := tx.Exec(t.Context(), `DELETE FROM computer_disk_roots WHERE id=$1`, root); err != nil {
					t.Fatal(err)
				}
				adopter, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer adopter.Rollback(context.Background())
				result := make(chan error, 1)
				go func() { result <- adopt(adopter) }()
				awaitRetentionLockWait(t, f, adopter)
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				requireRetentionFK(t, <-result)
			}
		})
	}
}

// The storage graph's relational arbitration is independent of byte inspection.
// Use schema-valid certified nodes to exercise the actual FK concurrency rather
// than replacing PostgreSQL with a graph model.
func graphRetentionObject(t *testing.T, f fixture, digit int, rank int) string {
	t.Helper()
	digest := fmt.Sprintf("sha256:%064x", digit)
	kind := "index"
	if rank == 0 {
		kind = "segment"
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,1);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$1,1,'application/octet-stream' FROM environments WHERE id=$2;
 INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$1,org_id,project_id,1,'application/octet-stream',$3,$4,'{}',clock_timestamp() FROM environments WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, digest, f.env, kind, rank)
	return digest
}

func TestDiskGraphPinsAndEdgesArbitrateDeletion(t *testing.T) {
	for _, kind := range []string{"pin", "edge"} {
		for _, adoptFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", kind, adoptFirst), func(t *testing.T) {
				f := newFixture(t)
				child := graphRetentionObject(t, f, 100, 0)
				parent := graphRetentionObject(t, f, 101, 1)
				_, save := f.finalize(t, "publication")
				// The parent itself remains owned, so collection cannot erase the adopter
				// merely because this test's graph is smaller than a complete disk.
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_object_pins(environment_id,save_id,digest) VALUES($1,$2,$3)`, f.env, save.ID, parent)
				tx, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				adopt := func(tx pgx.Tx) error {
					if kind == "pin" {
						_, err := tx.Exec(t.Context(), `INSERT INTO computer_object_pins(environment_id,save_id,digest) VALUES($1,$2,$3)`, f.env, save.ID, child)
						return err
					}
					_, err := tx.Exec(t.Context(), `INSERT INTO computer_object_edges(environment_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,1,0)`, f.env, parent, child)
					return err
				}
				if adoptFirst {
					if err := adopt(tx); err != nil {
						t.Fatal(err)
					}
					if err := reclaimComputerGraphRow(t.Context(), f.pool, `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2`, f.env, child); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err := collectComputerDiskGraph(t.Context(), f.pool); err != nil {
						t.Fatal(err)
					}
					var present bool
					if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2)`, f.env, child).Scan(&present); err != nil || !present {
						t.Fatalf("adopted object collected: %v %v", present, err)
					}
				} else {
					if _, err := tx.Exec(t.Context(), `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2`, f.env, child); err != nil {
						t.Fatal(err)
					}
					adopter, err := f.pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer adopter.Rollback(context.Background())
					result := make(chan error, 1)
					go func() { result <- adopt(adopter) }()
					awaitRetentionLockWait(t, f, adopter)
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
					requireRetentionFK(t, <-result)
				}
			})
		}
	}
}

func TestDiskGraphCollectionPreservesSharedChild(t *testing.T) {
	f := newFixture(t)
	child := graphRetentionObject(t, f, 110, 0)
	left := graphRetentionObject(t, f, 111, 1)
	right := graphRetentionObject(t, f, 112, 1)
	_, save := f.finalize(t, "shared-child")
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_object_edges(environment_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$4,1,0),($1,$3,$4,1,0);
 INSERT INTO computer_object_pins(environment_id,save_id,digest) VALUES($1,$5,$3)`, pgx.QueryExecModeSimpleProtocol, f.env, left, right, child, save.ID)
	for range 3 {
		if err := collectComputerDiskGraph(t.Context(), f.pool); err != nil {
			t.Fatal(err)
		}
	}
	var leftGone, rightRetained, childRetained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 NOT EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2),
 EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$3),
 EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$4)`, f.env, left, right, child).Scan(&leftGone, &rightRetained, &childRetained); err != nil || !leftGone || !rightRetained || !childRetained {
		t.Fatalf("left gone=%v right=%v child=%v error=%v", leftGone, rightRetained, childRetained, err)
	}
}

func awaitRetentionLockWait(t *testing.T, f fixture, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, tx.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("adopter did not wait for deletion")
		case <-time.After(time.Millisecond):
		}
	}
}
func requireRetentionFK(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("deleted dependency admitted: %v", err)
	}
}
