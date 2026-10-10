package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func saveObjectConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// ErrSaveStorageUnavailable reports that object storage could not confirm an
// uploaded object before its certification.
var ErrSaveStorageUnavailable = errors.New("computer object is not available in storage")

// saveObject is the typed description of one inspected disk object.
type saveObject struct {
	digest   string
	size     int64
	rank     int
	kind     string
	keys     []string
	nodes    []blockformat.NodeReference
	segments []blockformat.Ref
	encoded  []byte
}

func saveObjectDigest(d [32]byte) string { return "sha256:" + hex.EncodeToString(d[:]) }

func describeSaveObject(e blockformat.ObjectInspection) (saveObject, error) {
	var out saveObject
	if (e.Segment == nil) == (e.Pack == nil) {
		return out, saveStorageInput("one object inspection is required")
	}
	if e.Segment != nil {
		r := *e.Segment
		if r.Kind != blockformat.SegmentKind || r.Count == 0 || r.Count > blockformat.MaxRecords || r.Size <= 0 || r.Size > 5<<20 {
			return out, saveStorageInput("invalid segment inspection")
		}
		out.digest, out.size, out.kind = saveObjectDigest(r.Digest), r.Size, "segment"
		out.keys = []string{r.Key}
	} else {
		if len(e.Pack.Pages) == 0 {
			return out, saveStorageInput("empty pack inspection")
		}
		ref := e.Pack.Pages[0].Locator.Pack
		if ref.Size < 8 || ref.Size > 4<<20 || ref.Rank < 1 || ref.Rank > 6 {
			return out, saveStorageInput("invalid pack inspection")
		}
		out.digest, out.size, out.rank, out.kind = saveObjectDigest(ref.Digest), ref.Size, ref.Rank, "index"
		seen := map[string]bool{}
		for _, page := range e.Pack.Pages {
			if page.Locator.Pack != ref {
				return out, saveStorageInput("mixed pack inspection")
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
			return saveObject{}, saveStorageInput("invalid ciphertext key identity")
		}
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 16<<20 {
		return saveObject{}, saveStorageInput("inspection exceeds storage bounds")
	}
	out.encoded = raw
	return out, nil
}

// uploadedMatches reports whether stored bytes are the described object.
func (o saveObject) uploadedMatches(uploaded cas.Object) bool {
	return uploaded.Digest == o.digest && uploaded.SizeBytes == o.size && uploaded.MediaType == "application/octet-stream"
}

// saveObjectRetention ties an in-flight graph to either a Save and lease or
// a private preparation. Authority is established by the owning operation.
type saveObjectRetention struct {
	environmentID, computerID, saveID pgtype.UUID
	preparationID                     pgtype.UUID
	leaseEpoch                        int64
}

type saveObjectScope struct {
	saveObjectRetention
	orgID, projectID pgtype.UUID
	logicalBytes     int64
	allowedKeys      map[string]bool
	writeKey         string
	baseRoot         pgtype.UUID
}

func saveStorageInput(message string) error { return fmt.Errorf("%w: %s", ErrInvalidInput, message) }

// verifyRegistered checks, before storage I/O, that the object is registered
// with exactly this inspection and pinned for this publication.
func (r saveObjectRetention) verifyRegistered(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	object, err := describeSaveObject(inspection)
	if err != nil {
		return err
	}
	q := db.New(tx)
	row, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: r.environmentID, Digest: object.digest})
	if err != nil {
		return saveObjectMissing(err)
	}
	var stored blockformat.ObjectInspection
	if err := json.Unmarshal(row.Inspection, &stored); err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, inspection) {
		return saveObjectConflict("object differs from registered inspection")
	}
	return r.requireRetained(ctx, tx, object)
}

func (r saveObjectRetention) verifyCertified(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	if err := r.verifyRegistered(ctx, tx, inspection); err != nil {
		return err
	}
	object, err := describeSaveObject(inspection)
	if err != nil {
		return err
	}
	var certified bool
	if err = tx.QueryRow(ctx, `SELECT certified FROM computer_objects WHERE environment_id=$1 AND digest=$2`, r.environmentID, object.digest).Scan(&certified); err != nil {
		return err
	}
	if !certified {
		return ErrNotReady
	}
	return nil
}

// registerObject records an object before its upload: the blob lifetime and
// the Computer object with its inspection, the publication's pin, and, when
// the object is not yet certified, its dependency edges and key closure.
// Every child must be certified and admitted by this Computer lease or its retained source.
func (s saveObjectScope) registerObject(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	object, err := s.admit(inspection)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, object.digest, object.size); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,'application/octet-stream',$6,$7,$8) ON CONFLICT DO NOTHING`, s.environmentID, object.digest, s.orgID, s.projectID, object.size, object.kind, object.rank, object.encoded); err != nil {
		return err
	}
	row, err := s.lockMatching(ctx, tx, object, inspection)
	if err != nil {
		return err
	}
	if row.Certified.Bool {
		if err = s.requireAdmittedObject(ctx, tx, object.digest); err != nil {
			return err
		}
	}
	if err = s.pin(ctx, tx, object); err != nil {
		return err
	}
	if err = s.requireRetained(ctx, tx, object); err != nil {
		return err
	}
	if !row.Certified.Bool {
		if err = s.recordGraph(ctx, tx, object); err != nil {
			return err
		}
	}
	return s.checkKeyClosure(ctx, tx, object)
}

// certifyObject certifies a registered object once storage confirmed its
// uploaded bytes: it records the organization's CAS membership and marks the
// Computer object certified. It pins nothing; the registration's pin must
// still be retained.
func (s saveObjectScope) certifyObject(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection, uploaded cas.Object) error {
	object, err := describeSaveObject(inspection)
	if err != nil {
		return err
	}
	if !object.uploadedMatches(uploaded) {
		return saveObjectConflict("uploaded object descriptor mismatch")
	}
	if object, err = s.admit(inspection); err != nil {
		return err
	}
	row, err := s.lockMatching(ctx, tx, object, inspection)
	if err != nil {
		return err
	}
	if err = s.requireRetained(ctx, tx, object); err != nil {
		return err
	}
	if !row.Certified.Bool {
		if err = s.recordGraph(ctx, tx, object); err != nil {
			return err
		}
		q := db.New(tx)
		if _, err = q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: s.orgID, Digest: uploaded.Digest, SizeBytes: uploaded.SizeBytes, MediaType: uploaded.MediaType}); err != nil {
			return err
		}
		n, err := q.CertifyComputerObject(ctx, db.CertifyComputerObjectParams{EnvironmentID: s.environmentID, Digest: object.digest})
		if err != nil {
			return err
		}
		if n != 1 {
			return saveObjectConflict("object certification did not commit")
		}
	}
	return s.checkKeyClosure(ctx, tx, object)
}

// admit describes the object and checks its capacity and direct keys against
// the scope.
func (s saveObjectScope) admit(inspection blockformat.ObjectInspection) (saveObject, error) {
	object, err := describeSaveObject(inspection)
	if err != nil {
		return saveObject{}, err
	}
	if inspection.Pack != nil {
		for _, page := range inspection.Pack.Pages {
			if page.Shape.Capacity != s.logicalBytes {
				return saveObject{}, saveObjectConflict("object capacity differs from publication")
			}
		}
	}
	for _, id := range object.keys {
		if !s.allowedKeys[id] {
			return saveObject{}, saveObjectConflict("computer object key is not authorized for this publication")
		}
	}
	return object, nil
}

// lockMatching locks the Computer object and requires its stored facts to be
// exactly the described object.
func (s saveObjectScope) lockMatching(ctx context.Context, tx pgx.Tx, object saveObject, inspection blockformat.ObjectInspection) (db.ComputerObject, error) {
	row, err := db.New(tx).LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: s.environmentID, Digest: object.digest})
	if err != nil {
		return db.ComputerObject{}, saveObjectMissing(err)
	}
	var stored blockformat.ObjectInspection
	if err = json.Unmarshal(row.Inspection, &stored); err != nil {
		return db.ComputerObject{}, err
	}
	// JSONB normalizes representation, so compare decoded typed facts.
	if row.SizeBytes != object.size || row.Rank != int32(object.rank) || row.Kind != object.kind || row.MediaType != "application/octet-stream" || !reflect.DeepEqual(stored, inspection) {
		return db.ComputerObject{}, saveObjectConflict("object differs from registered inspection")
	}
	return row, nil
}

func (s saveObjectScope) pin(ctx context.Context, tx pgx.Tx, object saveObject) error {
	if s.preparationID.Valid {
		_, err := tx.Exec(ctx, `INSERT INTO computer_object_pins(environment_id,preparation_id,digest) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.environmentID, s.preparationID, object.digest)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO computer_object_pins(environment_id,save_id,digest) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.environmentID, s.saveID, object.digest)
	return err
}

func (r saveObjectRetention) requireRetained(ctx context.Context, tx pgx.Tx, object saveObject) error {
	var retained bool
	var err error
	if r.preparationID.Valid {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_object_pins WHERE environment_id=$1 AND preparation_id=$2 AND digest=$3)`, r.environmentID, r.preparationID, object.digest).Scan(&retained)
	} else {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_object_pins WHERE environment_id=$1 AND save_id=$2 AND digest=$3)`, r.environmentID, r.saveID, object.digest).Scan(&retained)
	}
	if err != nil {
		return err
	}
	if !retained {
		return saveObjectConflict("publication object is not retained")
	}
	return nil
}

// recordGraph resolves every physical reference of an uncertified object,
// including unselected pages, to an admitted certified child and
// records the edges and the object's direct keys. Missing dependencies fail
// before certification. Co-packed child pages share one read, and only one
// child's decoded evidence is retained at a time.
func (s saveObjectScope) recordGraph(ctx context.Context, tx pgx.Tx, object saveObject) error {
	for _, key := range object.keys {
		if key != s.writeKey {
			return saveObjectConflict("new computer object must use the active write key")
		}
	}
	packs := make(map[blockformat.PackRef][]blockformat.NodeReference)
	for _, child := range object.nodes {
		packs[child.Locator.Pack] = append(packs[child.Locator.Pack], child)
	}
	for ref, children := range packs {
		evidence, err := s.loadChild(ctx, tx, saveObjectDigest(ref.Digest), ref.Size, ref.Rank)
		if err != nil {
			return err
		}
		if evidence.Pack == nil {
			return saveObjectConflict("child pack inspection missing")
		}
		for _, child := range children {
			if err = evidence.Pack.CheckNode(child); err != nil {
				return saveObjectConflict("child node differs from its inspection: %v", err)
			}
		}
		if err = s.insertEdge(ctx, tx, object, saveObjectDigest(ref.Digest), ref.Rank); err != nil {
			return err
		}
	}
	segments := make(map[blockformat.Ref]struct{})
	for _, child := range object.segments {
		segments[child] = struct{}{}
	}
	for child := range segments {
		evidence, err := s.loadChild(ctx, tx, saveObjectDigest(child.Digest), child.Size, 0)
		if err != nil {
			return err
		}
		if evidence.Segment == nil || *evidence.Segment != child {
			return saveObjectConflict("child segment descriptor mismatch")
		}
		if err = s.insertEdge(ctx, tx, object, saveObjectDigest(child.Digest), 0); err != nil {
			return err
		}
	}
	for _, id := range object.keys {
		if _, err := tx.Exec(ctx, `INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$2,$3,true) ON CONFLICT DO NOTHING`, s.environmentID, object.digest, id); err != nil {
			return err
		}
	}
	return nil
}

// checkKeyClosure requires every key the object depends on to be retained by
// the publishing Computer lease.
func (s saveObjectScope) checkKeyClosure(ctx context.Context, tx pgx.Tx, object saveObject) error {
	rows, err := tx.Query(ctx, `SELECT key_id FROM computer_object_keys WHERE environment_id=$1 AND digest=$2`, s.environmentID, object.digest)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !s.allowedKeys[pgvalue.UUIDString(id)] {
			return saveObjectConflict("computer object depends on a key not authorized for this publication")
		}
	}
	return rows.Err()
}

func (s saveObjectScope) loadChild(ctx context.Context, tx pgx.Tx, digest string, size int64, rank int) (blockformat.ObjectInspection, error) {
	var raw []byte
	var storedSize int64
	var storedRank int
	var certified bool
	// KEY SHARE prevents collection until the edge's restrictive FK takes over.
	err := tx.QueryRow(ctx, `SELECT inspection,size_bytes,rank,certified FROM computer_objects WHERE environment_id=$1 AND digest=$2 FOR KEY SHARE`, s.environmentID, digest).Scan(&raw, &storedSize, &storedRank, &certified)
	if err != nil {
		return blockformat.ObjectInspection{}, saveObjectMissing(err)
	}
	if storedSize != size || storedRank != rank {
		return blockformat.ObjectInspection{}, saveObjectConflict("child object geometry differs from its reference")
	}
	if !certified {
		return blockformat.ObjectInspection{}, ErrNotReady
	}
	if err = s.requireAdmittedObject(ctx, tx, digest); err != nil {
		return blockformat.ObjectInspection{}, err
	}
	var evidence blockformat.ObjectInspection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&evidence)
	return evidence, err
}

func (s saveObjectScope) insertEdge(ctx context.Context, tx pgx.Tx, parent saveObject, digest string, rank int) error {
	_, err := tx.Exec(ctx, `INSERT INTO computer_object_edges(environment_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, s.environmentID, parent.digest, digest, parent.rank, rank)
	return err
}

func saveStorageUnavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrSaveStorageUnavailable, err)
}

// Digest knowledge is not authority. Reuse is limited to the mounted base's
// authenticated closure or objects retained by this same physical lease's saves.
// Private preparations have no incremental base: only their own pins are admitted.
func (s saveObjectScope) requireAdmittedObject(ctx context.Context, tx pgx.Tx, digest string) error {
	if s.preparationID.Valid {
		return s.requireRetained(ctx, tx, saveObject{digest: digest})
	}
	var admitted bool
	err := tx.QueryRow(ctx, `WITH RECURSIVE retained(digest,rank) AS (
      SELECT root_pack_digest,root_pack_rank FROM computer_disk_roots WHERE environment_id=$1 AND id=$5
    ), ancestors(digest) AS (
      SELECT $2::text UNION SELECT e.parent_digest FROM computer_object_edges e JOIN ancestors a ON e.child_digest=a.digest WHERE e.environment_id=$1
    ), source(digest,rank) AS (
      SELECT digest,rank FROM retained
      UNION SELECT e.child_digest,e.child_rank FROM computer_object_edges e JOIN source p ON e.parent_digest=p.digest
      WHERE e.environment_id=$1 AND p.rank>(SELECT rank FROM computer_objects WHERE environment_id=$1 AND digest=$2)
    ) SELECT CASE
      WHEN EXISTS(SELECT 1 FROM computer_object_pins p JOIN computer_saves v ON (v.environment_id,v.id)=(p.environment_id,p.save_id)
        WHERE p.environment_id=$1 AND p.digest=$2 AND v.computer_id=$3 AND v.computer_lease_epoch=$4) THEN true
      WHEN EXISTS(SELECT 1 FROM (SELECT digest FROM ancestors LIMIT 128) a JOIN retained r ON r.digest=a.digest) THEN true
      ELSE EXISTS(SELECT 1 FROM source WHERE digest=$2) END`, s.environmentID, digest, s.computerID, s.leaseEpoch, s.baseRoot).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return saveObjectConflict("object is outside the mounted base and lease publications")
	}
	return nil
}

func saveObjectMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotReady
	}
	return err
}
