package computer

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

// normalizeSaveBatch bounds a request before taking owner locks, and gives all
// transactions the same digest lock order. A batch never crosses dependency ranks.
func normalizeSaveBatch(input []blockformat.ObjectInspection) ([]blockformat.ObjectInspection, error) {
	if len(input) == 0 || len(input) > blockformat.MaxPublicationBatchObjects {
		return nil, ErrInvalidInput
	}
	type described struct {
		inspection blockformat.ObjectInspection
		object     saveObject
	}
	items := make([]described, len(input))
	bytes := 0
	for index, inspection := range input {
		object, err := describeSaveObject(inspection)
		if err != nil {
			return nil, err
		}
		bytes += len(object.encoded) + 1
		if bytes > blockformat.MaxPublicationBatchBytes || (index > 0 && object.rank != items[0].object.rank) {
			return nil, ErrInvalidInput
		}
		items[index] = described{inspection, object}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].object.digest < items[j].object.digest })
	out := make([]blockformat.ObjectInspection, len(input))
	for index, item := range items {
		if index > 0 && item.object.digest == items[index-1].object.digest {
			return nil, ErrInvalidInput
		}
		out[index] = item.inspection
	}
	return out, nil
}

type saveChild struct {
	inspection blockformat.ObjectInspection
	size       int64
	rank       int
}

// Resolve the union of a dependency level's children once. Restrictive edges
// preserve these key-share-locked rows after registration commits. Admission is
// checked as a set, while each incoming node position is checked independently.
func (s *saveObjectScope) prepareBatchChildren(ctx context.Context, tx pgx.Tx, inspections []blockformat.ObjectInspection) error {
	children := make(map[string]bool)
	for _, inspection := range inspections {
		object, err := s.admit(inspection)
		if err != nil {
			return err
		}
		for _, ref := range object.segments {
			children[saveObjectDigest(ref.Digest)] = true
		}
		for _, ref := range object.nodes {
			children[saveObjectDigest(ref.Locator.Pack.Digest)] = true
		}
	}
	s.children = make(map[string]saveChild, len(children))
	if len(children) == 0 {
		return nil
	}
	digests := make([]string, 0, len(children))
	for digest := range children {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	rows, err := tx.Query(ctx, `SELECT digest,inspection,size_bytes,rank,certified FROM disk_objects WHERE environment_id=$1 AND digest=ANY($2::text[]) ORDER BY digest FOR KEY SHARE`, s.environmentID, digests)
	if err != nil {
		return err
	}
	for rows.Next() {
		var digest string
		var raw []byte
		var child saveChild
		var certified bool
		if err = rows.Scan(&digest, &raw, &child.size, &child.rank, &certified); err != nil {
			break
		}
		if !certified {
			err = ErrNotReady
			break
		}
		if err = json.Unmarshal(raw, &child.inspection); err != nil {
			break
		}
		s.children[digest] = child
	}
	err = errors.Join(err, rows.Err())
	rows.Close()
	if err != nil {
		return err
	}
	if len(s.children) != len(digests) {
		return ErrNotReady
	}
	return s.requireAdmittedObjects(ctx, tx, digests)
}

func (s saveObjectScope) requireAdmittedObjects(ctx context.Context, tx pgx.Tx, digests []string) error {
	// Most children are pinned by this lease's preceding dependency level. Do
	// not traverse the base graph at all when that set already covers the request.
	rows, err := tx.Query(ctx, `SELECT requested.digest FROM unnest($2::text[]) requested(digest)
 WHERE NOT EXISTS(SELECT 1 FROM disk_object_pins p LEFT JOIN computer_saves v ON (v.environment_id,v.id)=(p.environment_id,p.save_id)
 WHERE p.environment_id=$1 AND p.digest=requested.digest AND
 (($5::uuid IS NOT NULL AND p.image_build_id=$5) OR ($5::uuid IS NULL AND v.computer_id=$3 AND v.computer_lease_epoch=$4)))`, s.environmentID, digests, s.computerID, s.leaseEpoch, s.buildID)
	if err != nil {
		return err
	}
	var missing []string
	for rows.Next() {
		var digest string
		if err = rows.Scan(&digest); err != nil {
			break
		}
		missing = append(missing, digest)
	}
	err = errors.Join(err, rows.Err())
	rows.Close()
	if err != nil || len(missing) == 0 {
		return err
	}
	if s.buildID.Valid {
		return saveObjectConflict("object is outside preparation publications")
	}
	var admitted bool
	err = tx.QueryRow(ctx, `WITH RECURSIVE source(digest,rank) AS (
 SELECT pack_digest,pack_rank FROM disk_roots WHERE environment_id=$1 AND
 (id=$2 OR id IN (SELECT root_id FROM computer_saves WHERE environment_id=$1 AND computer_id=$4 AND computer_lease_epoch=$5 AND status='published' AND root_id IS NOT NULL))
 UNION SELECT e.child_digest,e.child_rank FROM disk_object_edges e JOIN source p ON e.parent_digest=p.digest
 WHERE e.environment_id=$1 AND p.rank>(SELECT min(rank) FROM disk_objects WHERE environment_id=$1 AND digest=ANY($3::text[])))
 SELECT NOT EXISTS(SELECT 1 FROM unnest($3::text[]) wanted(digest) WHERE NOT EXISTS(SELECT 1 FROM source WHERE source.digest=wanted.digest))`, s.environmentID, s.baseRoot, missing, s.computerID, s.leaseEpoch).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return saveObjectConflict("object is outside the mounted base and lease publications")
	}
	return nil
}

func verifySaveBatch(ctx context.Context, store SaveObjectStore, inspections []blockformat.ObjectInspection) ([]cas.Object, error) {
	objects := make([]cas.Object, len(inspections))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for index, inspection := range inspections {
		group.Go(func() error {
			object, err := describeSaveObject(inspection)
			if err != nil {
				return err
			}
			objects[index], err = cas.VerifyStored(groupCtx, store, object.digest)
			if err != nil {
				return SaveStorageUnavailable(err)
			}
			return nil
		})
	}
	return objects, group.Wait()
}
