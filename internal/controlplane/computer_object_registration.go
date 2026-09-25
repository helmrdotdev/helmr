package controlplane

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type inspectedObject struct {
	digest   string
	size     int64
	rank     int
	kind     string
	keys     []string
	nodes    []blockformat.NodeReference
	segments []blockformat.Ref
	encoded  []byte
}

func objectDigest(d [32]byte) string { return "sha256:" + hex.EncodeToString(d[:]) }

func describeComputerObject(e blockformat.ObjectInspection) (inspectedObject, error) {
	var out inspectedObject
	if (e.Segment == nil) == (e.Pack == nil) {
		return out, errors.New("one object inspection is required")
	}
	if e.Segment != nil {
		r := *e.Segment
		if r.Kind != blockformat.SegmentKind || r.Count == 0 || r.Count > blockformat.MaxRecords || r.Size <= 0 || r.Size > 5<<20 {
			return out, errors.New("invalid segment inspection")
		}
		out.digest, out.size, out.kind = objectDigest(r.Digest), r.Size, "segment"
		out.keys = []string{r.Key}
	} else {
		if len(e.Pack.Pages) == 0 {
			return out, errors.New("empty pack inspection")
		}
		ref := e.Pack.Pages[0].Locator.Pack
		if ref.Size < 8 || ref.Size > 4<<20 || ref.Rank < 1 || ref.Rank > 6 {
			return out, errors.New("invalid pack inspection")
		}
		out.digest, out.size, out.rank, out.kind = objectDigest(ref.Digest), ref.Size, ref.Rank, "index"
		seen := map[string]bool{}
		for _, page := range e.Pack.Pages {
			if page.Locator.Pack != ref {
				return out, errors.New("mixed pack inspection")
			}
			if page.Locator.Page.Kind == blockformat.RootKind {
				out.kind = "root"
			}
			if !seen[page.Locator.Page.Key] {
				out.keys = append(out.keys, page.Locator.Page.Key)
				seen[page.Locator.Page.Key] = true
			}
			out.nodes = append(out.nodes, page.Children...)
			out.segments = append(out.segments, page.Segments...)
		}
	}
	for _, key := range out.keys {
		if _, err := uuid.Parse(key); err != nil {
			return inspectedObject{}, errors.New("invalid ciphertext key identity")
		}
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 16<<20 {
		return inspectedObject{}, errors.New("inspection exceeds storage bounds")
	}
	out.encoded = raw
	return out, nil
}

// recordInitialComputerObject registers before upload, then certifies only after
// exact CAS membership exists. Both operations revalidate live preparation and
// the pinned writer. This internal owner operation is initialization-specific;
// continuation must use its own authority, never this preparation fence.
// No remote/provider I/O runs under SQL locks. The caller owns staged bytes and
// may upload only after registration succeeds. No generation head is advanced.
func recordInitialComputerObject(ctx context.Context, dbtx TxBeginner, fence computerKeyFence, inspection blockformat.ObjectInspection, uploaded *cas.Object) error {
	object, err := describeComputerObject(inspection)
	if err != nil {
		return err
	}
	if uploaded != nil && (uploaded.Digest != object.digest || uploaded.SizeBytes != object.size || uploaded.MediaType != "application/octet-stream") {
		return errors.New("uploaded object descriptor mismatch")
	}
	tx, err := dbtx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	owner, err := dispatch.LockComputerPreparation(ctx, tx, fence.ComputerPreparationFence)
	if err != nil {
		return err
	}
	var claims bool
	if err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4 FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, fence.WorkerID, fence.WorkerGroupID, fence.ClaimVersion, fence.GroupClaimVersion).Scan(&claims); err != nil {
		return err
	}
	if !claims {
		return errors.New("object writer claims changed")
	}
	q := db.New(tx)
	key, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{RuntimeInstanceID: fence.RuntimeID, EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID})
	if err != nil {
		return err
	}
	var pinned bool
	if err = tx.QueryRow(ctx, `SELECT computer_write_key_id=$2 FROM runtime_instances WHERE id=$1`, fence.RuntimeID, key.ID).Scan(&pinned); err != nil {
		return err
	}
	if !pinned {
		return errors.New("initial writer key is not pinned")
	}
	if err = recordComputerObjectLocked(ctx, tx, owner, fence.RuntimeID, computerPublicationKey("initial", fence.RuntimeID, fence.RuntimeID), fence.DesiredVersion, inspection, uploaded, false, map[string]bool{pgvalue.UUIDString(key.ID): true}); err != nil {
		return err
	}
	if err = owner.CheckDeadlines(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The caller owns current preparation or checkpoint/finalization authority and
// rechecks deadlines after this operation. No remote I/O occurs under its locks.
func recordComputerObjectLocked(ctx context.Context, tx pgx.Tx, owner dispatch.ComputerPreparation, runtimeID pgtype.UUID, publicationKey []byte, desiredVersion int64, inspection blockformat.ObjectInspection, uploaded *cas.Object, reuse bool, allowedKeys map[string]bool) error {
	object, err := describeComputerObject(inspection)
	if err != nil {
		return err
	}
	if uploaded != nil && (uploaded.Digest != object.digest || uploaded.SizeBytes != object.size || uploaded.MediaType != "application/octet-stream") {
		return computerObjectConflict("uploaded object descriptor mismatch")
	}
	if inspection.Pack != nil {
		for _, page := range inspection.Pack.Pages {
			if page.Shape.Capacity != owner.LogicalBytes {
				return computerObjectConflict("object capacity differs from Computer")
			}
		}
	}
	for _, id := range object.keys {
		if !allowedKeys[id] {
			return computerObjectConflict("computer object key is not retained by Runtime")
		}
	}
	q := db.New(tx)
	if uploaded == nil && !reuse {
		if _, err = tx.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, object.digest, object.size); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,$6,'application/octet-stream',$7,$8,$9) ON CONFLICT DO NOTHING`, owner.EnvironmentID, owner.ComputerID, object.digest, owner.OrgID, owner.ProjectID, object.size, object.kind, object.rank, object.encoded); err != nil {
			return err
		}
	}
	row, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, Digest: object.digest})
	if err != nil {
		return err
	}
	var stored blockformat.ObjectInspection
	if err = json.Unmarshal(row.Inspection, &stored); err != nil {
		return err
	}
	// JSONB normalizes representation, so compare decoded typed facts.
	if row.SizeBytes != object.size || row.Rank != int32(object.rank) || row.Kind != object.kind || row.MediaType != "application/octet-stream" || !reflect.DeepEqual(stored, inspection) {
		return computerObjectConflict("object differs from registered inspection")
	}
	if reuse && !row.Certified.Bool {
		return computerObjectConflict("referenced computer object is not certified")
	}
	if uploaded == nil {
		if _, err = tx.Exec(ctx, `INSERT INTO runtime_computer_object_pins(runtime_instance_id,digest,environment_id,computer_id,runtime_desired_version,publication_key) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(runtime_instance_id,publication_key,digest) DO UPDATE SET runtime_desired_version=EXCLUDED.runtime_desired_version
 WHERE runtime_computer_object_pins.environment_id=EXCLUDED.environment_id
 AND runtime_computer_object_pins.computer_id=EXCLUDED.computer_id
 AND runtime_computer_object_pins.runtime_desired_version<=EXCLUDED.runtime_desired_version`, runtimeID, object.digest, owner.EnvironmentID, owner.ComputerID, desiredVersion, publicationKey); err != nil {
			return err
		}
	}
	var retained bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime_computer_object_pins WHERE runtime_instance_id=$1 AND digest=$2 AND runtime_desired_version=$3 AND publication_key=$4)`, runtimeID, object.digest, desiredVersion, publicationKey).Scan(&retained); err != nil {
		return err
	}
	if !retained {
		return computerObjectConflict("computer object candidate is not retained by Runtime")
	}
	if !row.Certified.Bool {
		// Missing dependencies fail before certification; all physical references,
		// including unselected pages, must resolve in the same Computer.
		// Group by physical identity: co-packed child pages share one DB read.
		// Retain only one child's decoded evidence at a time.
		packs := make(map[blockformat.PackRef][]blockformat.NodeReference)
		for _, child := range object.nodes {
			packs[child.Locator.Pack] = append(packs[child.Locator.Pack], child)
		}
		for ref, children := range packs {
			evidence, err := loadInspectedComputerChild(ctx, tx, owner, objectDigest(ref.Digest), ref.Size, ref.Rank)
			if err != nil {
				return err
			}
			if evidence.Pack == nil {
				return computerObjectConflict("child pack inspection missing")
			}
			for _, child := range children {
				if err = evidence.Pack.CheckNode(child); err != nil {
					return err
				}
			}
			if err = insertInspectedComputerEdge(ctx, tx, owner, object, objectDigest(ref.Digest), ref.Rank); err != nil {
				return err
			}
		}
		segments := make(map[blockformat.Ref]struct{})
		for _, child := range object.segments {
			segments[child] = struct{}{}
		}
		for child := range segments {
			evidence, err := loadInspectedComputerChild(ctx, tx, owner, objectDigest(child.Digest), child.Size, 0)
			if err != nil {
				return err
			}
			if evidence.Segment == nil || *evidence.Segment != child {
				return computerObjectConflict("child segment descriptor mismatch")
			}
			if err = insertInspectedComputerEdge(ctx, tx, owner, object, objectDigest(child.Digest), 0); err != nil {
				return err
			}
		}
		for _, id := range object.keys {
			if _, err = tx.Exec(ctx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true) ON CONFLICT DO NOTHING`, owner.EnvironmentID, owner.ComputerID, object.digest, id); err != nil {
				return err
			}
		}
		if uploaded != nil {

			if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: owner.OrgID, Digest: uploaded.Digest, SizeBytes: uploaded.SizeBytes, MediaType: uploaded.MediaType}); err != nil {
				return err
			}
			n, err := q.CertifyComputerObject(ctx, db.CertifyComputerObjectParams{EnvironmentID: owner.EnvironmentID, ComputerID: owner.ComputerID, Digest: object.digest})
			if err != nil {
				return err
			}
			if n != 1 {
				return computerObjectConflict("object certification did not commit")
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT key_id FROM computer_object_keys WHERE environment_id=$1 AND computer_id=$2 AND digest=$3`, owner.EnvironmentID, owner.ComputerID, object.digest)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !allowedKeys[pgvalue.UUIDString(id)] {
			return conflict(errors.New("computer object depends on a key not retained by Runtime"))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func loadInspectedComputerChild(ctx context.Context, tx pgx.Tx, owner dispatch.ComputerPreparation, digest string, size int64, rank int) (blockformat.ObjectInspection, error) {
	var raw []byte
	// KEY SHARE prevents collection until the edge's restrictive FK takes over.
	err := tx.QueryRow(ctx, `SELECT inspection FROM computer_objects WHERE environment_id=$1 AND computer_id=$2 AND digest=$3 AND size_bytes=$4 AND rank=$5 AND certified FOR KEY SHARE`, owner.EnvironmentID, owner.ComputerID, digest, size, rank).Scan(&raw)
	if err != nil {
		return blockformat.ObjectInspection{}, err
	}
	var evidence blockformat.ObjectInspection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&evidence)
	return evidence, err
}
func insertInspectedComputerEdge(ctx context.Context, tx pgx.Tx, owner dispatch.ComputerPreparation, parent inspectedObject, digest string, rank int) error {
	_, err := tx.Exec(ctx, `INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, owner.EnvironmentID, owner.ComputerID, parent.digest, digest, parent.rank, rank)
	return err
}

func computerObjectConflict(message string) error { return conflict(errors.New(message)) }
