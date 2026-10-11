package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Attachment and dependency FKs arbitrate adoption against deletion. Each
// candidate has its own transaction so one concurrent adopter cannot roll back
// unrelated progress. Deleting an object also drops only its outgoing edges and
// key summaries; children remain protected by every other parent.
func collectComputerDiskGraph(ctx context.Context, database db.TxDB) error {
	rows, err := database.Query(ctx, `SELECT r.id FROM computer_disk_roots r
 WHERE NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=r.environment_id AND s.root_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM computers c WHERE c.environment_id=r.environment_id AND c.initial_root_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=r.environment_id AND l.retained_base_root_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM computer_images i WHERE i.environment_id=r.environment_id AND i.root_id=r.id)
 ORDER BY r.id LIMIT 100 FOR UPDATE OF r SKIP LOCKED`)
	if err != nil {
		return err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range roots {
		if err := reclaimComputerGraphRow(ctx, database, `DELETE FROM computer_disk_roots WHERE id=$1`, id); err != nil {
			failures = append(failures, err)
		}
	}
	rows, err = database.Query(ctx, `SELECT o.environment_id,o.digest FROM computer_objects o
 WHERE NOT EXISTS(SELECT 1 FROM computer_disk_roots r WHERE r.environment_id=o.environment_id AND r.root_pack_digest=o.digest)
 AND NOT EXISTS(SELECT 1 FROM computer_object_pins p WHERE p.environment_id=o.environment_id AND p.digest=o.digest)
 AND NOT EXISTS(SELECT 1 FROM computer_object_edges e WHERE e.environment_id=o.environment_id AND e.child_digest=o.digest)
 ORDER BY o.rank DESC,o.environment_id,o.digest LIMIT 100 FOR UPDATE OF o SKIP LOCKED`)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	type object struct {
		environment uuid.UUID
		digest      string
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object, error) {
		var o object
		err := row.Scan(&o.environment, &o.digest)
		return o, err
	})
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, o := range objects {
		if err := reclaimComputerObject(ctx, database, o.environment, o.digest); err != nil {
			failures = append(failures, err)
		}
	}
	rows, err = database.Query(ctx, `SELECT k.id FROM computer_data_keys k WHERE k.available
 AND NOT EXISTS(SELECT 1 FROM computer_object_keys r WHERE r.environment_id=k.environment_id AND r.key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=k.environment_id AND l.write_key_id=k.id AND l.disk_released_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_preparations p WHERE p.environment_id=k.environment_id AND p.write_key_id=k.id AND p.disk_released_at IS NULL)
 ORDER BY k.id LIMIT 100 FOR UPDATE OF k SKIP LOCKED`)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, id := range keys {
		if err := reclaimComputerGraphRow(ctx, database, `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1 AND available`, id); err != nil {
			failures = append(failures, err)
		}
	}
	// Deployment dependencies and other tenant graph/checkpoint owners retain
	// their memberships independently of any collected Computer object.
	return errors.Join(failures...)
}

func attachmentWon(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// FK arbitration can wait for an adopter that started after discovery. Its
// bounded lock wait must not consume the collection pass for unrelated rows.
func reclaimComputerGraphRow(ctx context.Context, database db.TxBeginner, query string, args ...any) error {
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, query, args...)
		return err
	})
	var pgErr *pgconn.PgError
	if attachmentWon(err) || (errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		return nil
	}
	return err
}

func reclaimComputerObject(ctx context.Context, database db.TxBeginner, environment uuid.UUID, digest string) error {
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
			return err
		}
		var org uuid.UUID
		if err := tx.QueryRow(ctx, `DELETE FROM computer_objects WHERE environment_id=$1 AND digest=$2 RETURNING org_id`, environment, digest).Scan(&org); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		return releaseComputerCASMembership(ctx, tx, org, digest)
	})
	var pgErr *pgconn.PgError
	if attachmentWon(err) || (errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		return nil
	}
	return err
}
