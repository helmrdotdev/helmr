// Package authority is a development-only PostgreSQL publication/retention model.
// Its operations are test fixtures, not Product APIs or a trusted certifier.
package authority

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

var errConflict = errors.New("authority or immutable identity conflict")

type publication struct {
	Env, ID, Computer           string
	Epoch                       int64
	Source, Checkpoint, Capture string
}
type manifest struct {
	ID, Root         string
	Offset, Capacity int64
}
type object struct {
	Digest, Kind string
	Rank         int
	Size         int64
}
type store struct{ pool *pgxpool.Pool }

func (s store) transaction(ctx context.Context, fn func(pgx.Tx) error) error {
	_, err := retry(ctx, 3, func() error { return s.transactionOnce(ctx, fn) })
	return err
}
func (s store) transactionOnce(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Retry the entire transaction, never a statement on an aborted transaction.
// Callers decide the finite budget; exhaustion preserves the SQLSTATE for logging.
func retry(ctx context.Context, budget int, fn func() error) (int, error) {
	if budget < 1 {
		return 0, errors.New("empty transaction retry budget")
	}
	for n := 1; n <= budget; n++ {
		if err := ctx.Err(); err != nil {
			return n - 1, err
		}
		err := fn()
		var pg *pgconn.PgError
		if err == nil || !errors.As(err, &pg) || (pg.Code != "40P01" && pg.Code != "40001") {
			return n, err
		}
		if n == budget {
			return n, fmt.Errorf("transaction retry exhausted after %d attempts: %w", n, err)
		}
	}
	panic("unreachable")
}
func exec(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	_, err := tx.Exec(ctx, sql, args...)
	return err
}

func lockComputer(ctx context.Context, tx pgx.Tx, p publication) (int64, string, error) {
	var epoch int64
	var head string
	err := tx.QueryRow(ctx, `SELECT epoch,coalesce(head_id,'') FROM computers WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.Env, p.Computer).Scan(&epoch, &head)
	return epoch, head, err
}
func samePublication(ctx context.Context, tx pgx.Tx, p publication) (string, error) {
	var got publication
	var status string
	err := tx.QueryRow(ctx, `SELECT computer_id,epoch,coalesce(predecessor_id,''),coalesce(checkpoint_id,''),coalesce(capture_id,''),status FROM computer_publications WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.Env, p.ID).Scan(&got.Computer, &got.Epoch, &got.Source, &got.Checkpoint, &got.Capture, &status)
	if err != nil {
		return "", err
	}
	if got.Computer != p.Computer || got.Epoch != p.Epoch || got.Source != p.Source || got.Checkpoint != p.Checkpoint || got.Capture != p.Capture {
		return "", errConflict
	}
	return status, nil
}
func (s store) begin(ctx context.Context, p publication) error {
	return s.transaction(ctx, func(tx pgx.Tx) error { return begin(ctx, tx, p) })
}
func begin(ctx context.Context, tx pgx.Tx, p publication) error {
	epoch, head, err := lockComputer(ctx, tx, p)
	if err != nil {
		return err
	}
	if _, err = samePublication(ctx, tx, p); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if epoch != p.Epoch || head != p.Source {
		return errConflict
	}
	if p.Checkpoint != "" {
		var matches bool
		err = tx.QueryRow(ctx, `SELECT computer_id=$3 AND epoch=$4 AND capture_id=$5 AND version_id IS NULL FROM checkpoints WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.Env, p.Checkpoint, p.Computer, p.Epoch, p.Capture).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return errConflict
		}
	}
	return exec(ctx, tx, `INSERT INTO computer_publications(environment_id,id,computer_id,epoch,predecessor_id,source_pin,checkpoint_id,capture_id,status) VALUES($1,$2,$3,$4,nullif($5,''),nullif($5,''),nullif($6,''),nullif($7,''),'constructing')`, p.Env, p.ID, p.Computer, p.Epoch, p.Source, p.Checkpoint, p.Capture)
}
func (s store) admit(ctx context.Context, p publication, o object) error {
	return s.transaction(ctx, func(tx pgx.Tx) error { return admit(ctx, tx, p, o) })
}
func admit(ctx context.Context, tx pgx.Tx, p publication, o object) error {
	status, err := samePublication(ctx, tx, p)
	if err != nil {
		return err
	}
	if status != "constructing" {
		return errConflict
	}
	if err = exec(ctx, tx, `INSERT INTO cas_object_lifetimes(digest) VALUES($1) ON CONFLICT DO NOTHING`, o.Digest); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,key_version) SELECT id,$2,org_id,project_id,$3,'application/proof',$4,$5,1 FROM environments WHERE id=$1 ON CONFLICT DO NOTHING`, p.Env, o.Digest, o.Size, o.Kind, o.Rank)
	if err != nil {
		return err
	}
	var matches bool
	err = tx.QueryRow(ctx, `SELECT size_bytes=$3 AND kind=$4 AND rank=$5 AND key_version=1 AND media_type='application/proof' FROM computer_objects WHERE environment_id=$1 AND digest=$2`, p.Env, o.Digest, o.Size, o.Kind, o.Rank).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return errConflict
	}
	return exec(ctx, tx, `INSERT INTO computer_publication_objects VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.Env, p.ID, o.Digest)
}
func (s store) seal(ctx context.Context, p publication, m manifest) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		status, err := samePublication(ctx, tx, p)
		if err != nil {
			return err
		}
		if status == "registered" || status == "published" {
			return sameManifest(ctx, tx, p, m)
		}
		if status != "constructing" {
			return errConflict
		}
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_publication_objects WHERE environment_id=$1 AND publication_id=$2 AND digest=$3)`, p.Env, p.ID, m.Root).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return errConflict
		}
		return exec(ctx, tx, `UPDATE computer_publications SET status='registered',manifest=$3,root_digest=$4,page_offset=$5,capacity=$6 WHERE environment_id=$1 AND id=$2`, p.Env, p.ID, m.ID, m.Root, m.Offset, m.Capacity)
	})
}
func sameManifest(ctx context.Context, tx pgx.Tx, p publication, m manifest) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT manifest=$3 AND root_digest=$4 AND page_offset=$5 AND capacity=$6 FROM computer_publications WHERE environment_id=$1 AND id=$2`, p.Env, p.ID, m.ID, m.Root, m.Offset, m.Capacity).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return errConflict
	}
	return nil
}

// children is a trusted verifier fixture. These tests do NOT verify bytes, AEAD,
// page membership or geometry; those are separate codec/certifier proof boundaries.
func (s store) certify(ctx context.Context, p publication, o object, children []string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		status, err := samePublication(ctx, tx, p)
		if err != nil {
			return err
		}
		if status != "constructing" && status != "registered" {
			return errConflict
		}
		var certified, member bool
		var org string
		var rank int
		var size int64
		var kind string
		err = tx.QueryRow(ctx, `SELECT o.certified,o.org_id,o.rank,o.size_bytes,o.kind,EXISTS(SELECT 1 FROM computer_publication_objects p WHERE p.environment_id=o.environment_id AND p.digest=o.digest AND p.publication_id=$3) FROM computer_objects o WHERE environment_id=$1 AND digest=$2 FOR UPDATE`, p.Env, o.Digest, p.ID).Scan(&certified, &org, &rank, &size, &kind, &member)
		if err != nil {
			return err
		}
		if !member || rank != o.Rank || size != o.Size || kind != o.Kind {
			return errConflict
		}
		children = slices.Clone(children)
		slices.Sort(children)
		if len(slices.Compact(slices.Clone(children))) != len(children) {
			return errConflict
		}
		if certified {
			rows, err := tx.Query(ctx, `SELECT child_digest FROM computer_object_edges WHERE environment_id=$1 AND parent_digest=$2 ORDER BY child_digest`, p.Env, o.Digest)
			if err != nil {
				return err
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if !slices.Equal(got, children) {
				return errConflict
			}
			return nil
		}
		var digest string
		err = tx.QueryRow(ctx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/proof') ON CONFLICT(org_id,digest) DO UPDATE SET size_bytes=cas_objects.size_bytes WHERE cas_objects.size_bytes=excluded.size_bytes AND cas_objects.media_type=excluded.media_type RETURNING digest`, org, o.Digest, o.Size).Scan(&digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return errConflict
		}
		if err != nil {
			return err
		}
		for _, child := range children {
			var childRank int
			err = tx.QueryRow(ctx, `SELECT o.rank FROM computer_objects o JOIN computer_publication_objects p USING(environment_id,digest) WHERE o.environment_id=$1 AND o.digest=$2 AND o.certified AND p.publication_id=$3`, p.Env, child, p.ID).Scan(&childRank)
			if err != nil {
				return err
			}
			if err = exec(ctx, tx, `INSERT INTO computer_object_edges(environment_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,$4,$5)`, p.Env, o.Digest, child, rank, childRank); err != nil {
				return err
			}
		}
		return exec(ctx, tx, `UPDATE computer_objects SET certified_at=clock_timestamp() WHERE environment_id=$1 AND digest=$2`, p.Env, o.Digest)
	})
}
func (s store) publish(ctx context.Context, p publication, m manifest) (string, error) {
	var receipt string
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		epoch, head, err := lockComputer(ctx, tx, p)
		if err != nil {
			return err
		}
		status, err := samePublication(ctx, tx, p)
		if err != nil {
			return err
		}
		if err = sameManifest(ctx, tx, p, m); err != nil {
			return err
		}
		if status == "published" {
			return tx.QueryRow(ctx, `SELECT result_id FROM computer_publications WHERE environment_id=$1 AND id=$2`, p.Env, p.ID).Scan(&receipt)
		}
		if status != "registered" || epoch != p.Epoch {
			return errConflict
		}
		if p.Checkpoint == "" {
			if head != p.Source {
				return errConflict
			}
		} else {
			var valid bool
			err = tx.QueryRow(ctx, `SELECT computer_id=$3 AND epoch=$4 AND capture_id=$5 AND version_id IS NULL FROM checkpoints WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.Env, p.Checkpoint, p.Computer, p.Epoch, p.Capture).Scan(&valid)
			if err != nil {
				return err
			}
			if !valid {
				return errConflict
			}
		}
		// The authenticated caller and trusted verified root locator are fixture premises.
		receipt = "version-" + p.ID
		if err = exec(ctx, tx, `INSERT INTO computer_versions VALUES($1,$2,nullif($3,''))`, p.Env, receipt, p.Source); err != nil {
			return err
		}
		if err = exec(ctx, tx, `INSERT INTO computer_version_roots(environment_id,version_id,digest,page_offset,capacity) VALUES($1,$2,$3,$4,$5)`, p.Env, receipt, m.Root, m.Offset, m.Capacity); err != nil {
			return err
		}
		if p.Checkpoint == "" {
			err = exec(ctx, tx, `UPDATE computers SET head_id=$3 WHERE environment_id=$1 AND id=$2`, p.Env, p.Computer, receipt)
		} else {
			err = exec(ctx, tx, `UPDATE checkpoints SET version_id=$3 WHERE environment_id=$1 AND id=$2`, p.Env, p.Checkpoint, receipt)
		}
		if err != nil {
			return err
		}
		if err = exec(ctx, tx, `UPDATE computer_publications SET status='published',result_id=$3,source_pin=NULL WHERE environment_id=$1 AND id=$2`, p.Env, p.ID, receipt); err != nil {
			return err
		}
		return exec(ctx, tx, `DELETE FROM computer_publication_objects WHERE environment_id=$1 AND publication_id=$2`, p.Env, p.ID)
	})
	if err != nil {
		return "", err
	}
	return receipt, nil
}
func abandon(ctx context.Context, tx pgx.Tx, p publication) error {
	status, err := samePublication(ctx, tx, p)
	if err != nil {
		return err
	}
	if status == "published" {
		return errConflict
	}
	if err = exec(ctx, tx, `DELETE FROM computer_publication_objects WHERE environment_id=$1 AND publication_id=$2`, p.Env, p.ID); err != nil {
		return err
	}
	return exec(ctx, tx, `UPDATE computer_publications SET status='abandoned',source_pin=NULL WHERE environment_id=$1 AND id=$2`, p.Env, p.ID)
}
func (s store) abandon(ctx context.Context, p publication) error {
	return s.transaction(ctx, func(tx pgx.Tx) error { return abandon(ctx, tx, p) })
}
func (s store) supersede(ctx context.Context, old, next publication) error {
	if old.ID == next.ID || old.Env != next.Env || old.Computer != next.Computer || old.Source != next.Source || old.Epoch != next.Epoch || old.Checkpoint != "" || next.Checkpoint != "" {
		return errConflict
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockComputer(ctx, tx, old); err != nil {
			return err
		}
		status, err := samePublication(ctx, tx, old)
		if err != nil {
			return err
		}
		if status != "constructing" && status != "registered" {
			return errConflict
		}
		// A successor must be new: an existing published/abandoned candidate is
		// never reactivated by transferring work pins into it.
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_publications WHERE environment_id=$1 AND id=$2)`, next.Env, next.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errConflict
		}
		if err = begin(ctx, tx, next); err != nil {
			return err
		}
		if err = exec(ctx, tx, `INSERT INTO computer_publication_objects SELECT environment_id,$3,digest FROM computer_publication_objects WHERE environment_id=$1 AND publication_id=$2 ON CONFLICT DO NOTHING`, old.Env, old.ID, next.ID); err != nil {
			return err
		}
		return abandon(ctx, tx, old)
	})
}

// One digest is a bounded collection batch. Lifetime rows are permanent cleanup
// discovery/tombstones, including objects uploaded after an earlier physical delete.
func (s store) collect(ctx context.Context, digest string, afterGraph func(pgx.Tx) error) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := exec(ctx, tx, `DELETE FROM computer_objects o WHERE digest=$1
    AND NOT EXISTS(SELECT 1 FROM computer_version_roots r WHERE r.environment_id=o.environment_id AND r.digest=o.digest)
    AND NOT EXISTS(SELECT 1 FROM computer_publication_objects p WHERE p.environment_id=o.environment_id AND p.digest=o.digest)
    AND NOT EXISTS(SELECT 1 FROM computer_object_edges e WHERE e.environment_id=o.environment_id AND e.child_digest=o.digest)`, digest); err != nil {
			return err
		}
		if afterGraph != nil {
			if err := afterGraph(tx); err != nil {
				return err
			}
		}
		// Lock memberships before checking artifact ownership: a concurrent artifact
		// insert takes KEY SHARE; its cascade cannot be authorized by a stale snapshot.
		rows, err := tx.Query(ctx, `SELECT org_id FROM cas_objects WHERE digest=$1 ORDER BY org_id FOR UPDATE`, digest)
		if err != nil {
			return err
		}
		orgs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, org := range orgs {
			if err = exec(ctx, tx, `DELETE FROM cas_objects c WHERE org_id=$1 AND digest=$2
    AND NOT EXISTS(SELECT 1 FROM computer_objects o WHERE o.org_id=c.org_id AND o.digest=c.digest)
    AND NOT EXISTS(SELECT 1 FROM artifacts a WHERE a.org_id=c.org_id AND a.digest=c.digest)`, org, digest); err != nil {
				return err
			}
		}
		var available bool
		if err = tx.QueryRow(ctx, `SELECT available FROM cas_object_lifetimes WHERE digest=$1 FOR UPDATE`, digest).Scan(&available); err != nil {
			return err
		}
		if !available {
			return nil
		}
		return exec(ctx, tx, `UPDATE cas_object_lifetimes l SET retired_at=clock_timestamp() WHERE digest=$1
   AND NOT EXISTS(SELECT 1 FROM computer_objects o WHERE o.digest=l.digest)
   AND NOT EXISTS(SELECT 1 FROM cas_objects c WHERE c.digest=l.digest)`, digest)
	})
}
