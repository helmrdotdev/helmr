package computer

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

// ObjectConflictError reports a disk object whose registration, stored bytes
// or dependencies differ from what its publication requires.
type ObjectConflictError struct {
	message string
}

func (e ObjectConflictError) Error() string {
	return e.message
}

func objectConflict(format string, args ...any) error {
	return ObjectConflictError{message: fmt.Sprintf(format, args...)}
}

// ErrStorageUnavailable reports that object storage could not confirm an
// uploaded object before its certification.
var ErrStorageUnavailable = errors.New("computer object is not available in storage")

// inspectedObject is the typed description of one inspected disk object.
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

// ValidateObjectInspection checks that a worker-supplied inspection describes
// exactly one disk object within storage bounds. It reports an InputError.
func ValidateObjectInspection(inspection blockformat.ObjectInspection) error {
	_, err := describeObject(inspection)
	return err
}

func describeObject(e blockformat.ObjectInspection) (inspectedObject, error) {
	var out inspectedObject
	if (e.Segment == nil) == (e.Pack == nil) {
		return out, invalidInput("one object inspection is required")
	}
	if e.Segment != nil {
		r := *e.Segment
		if r.Kind != blockformat.SegmentKind || r.Count == 0 || r.Count > blockformat.MaxRecords || r.Size <= 0 || r.Size > 5<<20 {
			return out, invalidInput("invalid segment inspection")
		}
		out.digest, out.size, out.kind = objectDigest(r.Digest), r.Size, "segment"
		out.keys = []string{r.Key}
	} else {
		if len(e.Pack.Pages) == 0 {
			return out, invalidInput("empty pack inspection")
		}
		ref := e.Pack.Pages[0].Locator.Pack
		if ref.Size < 8 || ref.Size > 4<<20 || ref.Rank < 1 || ref.Rank > 6 {
			return out, invalidInput("invalid pack inspection")
		}
		out.digest, out.size, out.rank, out.kind = objectDigest(ref.Digest), ref.Size, ref.Rank, "index"
		seen := map[string]bool{}
		for _, page := range e.Pack.Pages {
			if page.Locator.Pack != ref {
				return out, invalidInput("mixed pack inspection")
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
			return inspectedObject{}, invalidInput("invalid ciphertext key identity")
		}
	}
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 16<<20 {
		return inspectedObject{}, invalidInput("inspection exceeds storage bounds")
	}
	out.encoded = raw
	return out, nil
}

// uploadedMatches reports whether stored bytes are the described object.
func (o inspectedObject) uploadedMatches(uploaded cas.Object) bool {
	return uploaded.Digest == o.digest && uploaded.SizeBytes == o.size && uploaded.MediaType == "application/octet-stream"
}

// objectRetention is where one publication's candidate pins live: its
// Computer, the publishing Instance at its desired version, and the
// publication key.
type objectRetention struct {
	environmentID, computerID, instanceID pgtype.UUID
	desiredVersion                        int64
	key                                   publicationKey
}

// objectScope is the object recording scope of one publication, built only
// by the authority that holds its locks: the retention of its pins, the
// Computer's owner and capacity, and the key identities the publishing
// Instance retains. The caller rechecks its authority after recording and
// runs no remote I/O under its locks.
type objectScope struct {
	objectRetention
	orgID, projectID pgtype.UUID
	logicalBytes     int64
	allowedKeys      map[string]bool
}

// verifyRegistered checks, before storage I/O, that the object is registered
// with exactly this inspection and pinned for this publication.
func (r objectRetention) verifyRegistered(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	q := db.New(tx)
	row, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: r.environmentID, ComputerID: r.computerID, Digest: object.digest})
	if err != nil {
		return err
	}
	var stored blockformat.ObjectInspection
	if err := json.Unmarshal(row.Inspection, &stored); err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, inspection) {
		return objectConflict("object differs from registered inspection")
	}
	_, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: r.instanceID, PublicationKey: r.key, InstanceDesiredVersion: r.desiredVersion, Digest: object.digest})
	return err
}

// registerObject records an object before its upload: the blob lifetime and
// the Computer object with its inspection, the publication's pin, and, when
// the object is not yet certified, its dependency edges and key closure.
// Every child must already be certified in the same Computer.
func (s objectScope) registerObject(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	object, err := s.admit(inspection)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, object.digest, object.size); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,$6,'application/octet-stream',$7,$8,$9) ON CONFLICT DO NOTHING`, s.environmentID, s.computerID, object.digest, s.orgID, s.projectID, object.size, object.kind, object.rank, object.encoded); err != nil {
		return err
	}
	row, err := s.lockMatching(ctx, tx, object, inspection)
	if err != nil {
		return err
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
func (s objectScope) certifyObject(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection, uploaded cas.Object) error {
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	if !object.uploadedMatches(uploaded) {
		return objectConflict("uploaded object descriptor mismatch")
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
		n, err := q.CertifyComputerObject(ctx, db.CertifyComputerObjectParams{EnvironmentID: s.environmentID, ComputerID: s.computerID, Digest: object.digest})
		if err != nil {
			return err
		}
		if n != 1 {
			return objectConflict("object certification did not commit")
		}
	}
	return s.checkKeyClosure(ctx, tx, object)
}

// reuseObject pins an already certified object of the same Computer for this
// publication without uploading it again.
func (s objectScope) reuseObject(ctx context.Context, tx pgx.Tx, inspection blockformat.ObjectInspection) error {
	object, err := s.admit(inspection)
	if err != nil {
		return err
	}
	row, err := s.lockMatching(ctx, tx, object, inspection)
	if err != nil {
		return err
	}
	if !row.Certified.Bool {
		return objectConflict("referenced computer object is not certified")
	}
	if err = s.pin(ctx, tx, object); err != nil {
		return err
	}
	if err = s.requireRetained(ctx, tx, object); err != nil {
		return err
	}
	return s.checkKeyClosure(ctx, tx, object)
}

// admit describes the object and checks its capacity and direct keys against
// the scope.
func (s objectScope) admit(inspection blockformat.ObjectInspection) (inspectedObject, error) {
	object, err := describeObject(inspection)
	if err != nil {
		return inspectedObject{}, err
	}
	if inspection.Pack != nil {
		for _, page := range inspection.Pack.Pages {
			if page.Shape.Capacity != s.logicalBytes {
				return inspectedObject{}, objectConflict("object capacity differs from Computer")
			}
		}
	}
	for _, id := range object.keys {
		if !s.allowedKeys[id] {
			return inspectedObject{}, objectConflict("computer object key is not retained by the Instance")
		}
	}
	return object, nil
}

// lockMatching locks the Computer object and requires its stored facts to be
// exactly the described object.
func (s objectScope) lockMatching(ctx context.Context, tx pgx.Tx, object inspectedObject, inspection blockformat.ObjectInspection) (db.ComputerObject, error) {
	row, err := db.New(tx).LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: s.environmentID, ComputerID: s.computerID, Digest: object.digest})
	if err != nil {
		return db.ComputerObject{}, err
	}
	var stored blockformat.ObjectInspection
	if err = json.Unmarshal(row.Inspection, &stored); err != nil {
		return db.ComputerObject{}, err
	}
	// JSONB normalizes representation, so compare decoded typed facts.
	if row.SizeBytes != object.size || row.Rank != int32(object.rank) || row.Kind != object.kind || row.MediaType != "application/octet-stream" || !reflect.DeepEqual(stored, inspection) {
		return db.ComputerObject{}, objectConflict("object differs from registered inspection")
	}
	return row, nil
}

func (s objectScope) pin(ctx context.Context, tx pgx.Tx, object inspectedObject) error {
	_, err := tx.Exec(ctx, `INSERT INTO computer_object_pins(computer_instance_id,digest,environment_id,computer_id,instance_desired_version,publication_key) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(computer_instance_id,publication_key,digest) DO UPDATE SET instance_desired_version=EXCLUDED.instance_desired_version
 WHERE computer_object_pins.environment_id=EXCLUDED.environment_id
 AND computer_object_pins.computer_id=EXCLUDED.computer_id
 AND computer_object_pins.instance_desired_version<=EXCLUDED.instance_desired_version`, s.instanceID, object.digest, s.environmentID, s.computerID, s.desiredVersion, []byte(s.key))
	return err
}

func (s objectScope) requireRetained(ctx context.Context, tx pgx.Tx, object inspectedObject) error {
	var retained bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_object_pins WHERE computer_instance_id=$1 AND digest=$2 AND instance_desired_version=$3 AND publication_key=$4)`, s.instanceID, object.digest, s.desiredVersion, []byte(s.key)).Scan(&retained); err != nil {
		return err
	}
	if !retained {
		return objectConflict("computer object candidate is not retained by the Instance")
	}
	return nil
}

// recordGraph resolves every physical reference of an uncertified object,
// including unselected pages, to a certified child in the same Computer and
// records the edges and the object's direct keys. Missing dependencies fail
// before certification. Co-packed child pages share one read, and only one
// child's decoded evidence is retained at a time.
func (s objectScope) recordGraph(ctx context.Context, tx pgx.Tx, object inspectedObject) error {
	packs := make(map[blockformat.PackRef][]blockformat.NodeReference)
	for _, child := range object.nodes {
		packs[child.Locator.Pack] = append(packs[child.Locator.Pack], child)
	}
	for ref, children := range packs {
		evidence, err := s.loadChild(ctx, tx, objectDigest(ref.Digest), ref.Size, ref.Rank)
		if err != nil {
			return err
		}
		if evidence.Pack == nil {
			return objectConflict("child pack inspection missing")
		}
		for _, child := range children {
			if err = evidence.Pack.CheckNode(child); err != nil {
				return objectConflict("child node differs from its inspection: %v", err)
			}
		}
		if err = s.insertEdge(ctx, tx, object, objectDigest(ref.Digest), ref.Rank); err != nil {
			return err
		}
	}
	segments := make(map[blockformat.Ref]struct{})
	for _, child := range object.segments {
		segments[child] = struct{}{}
	}
	for child := range segments {
		evidence, err := s.loadChild(ctx, tx, objectDigest(child.Digest), child.Size, 0)
		if err != nil {
			return err
		}
		if evidence.Segment == nil || *evidence.Segment != child {
			return objectConflict("child segment descriptor mismatch")
		}
		if err = s.insertEdge(ctx, tx, object, objectDigest(child.Digest), 0); err != nil {
			return err
		}
	}
	for _, id := range object.keys {
		if _, err := tx.Exec(ctx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true) ON CONFLICT DO NOTHING`, s.environmentID, s.computerID, object.digest, id); err != nil {
			return err
		}
	}
	return nil
}

// checkKeyClosure requires every key the object depends on to be retained by
// the publishing Instance.
func (s objectScope) checkKeyClosure(ctx context.Context, tx pgx.Tx, object inspectedObject) error {
	rows, err := tx.Query(ctx, `SELECT key_id FROM computer_object_keys WHERE environment_id=$1 AND computer_id=$2 AND digest=$3`, s.environmentID, s.computerID, object.digest)
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
			return objectConflict("computer object depends on a key not retained by the Instance")
		}
	}
	return rows.Err()
}

func (s objectScope) loadChild(ctx context.Context, tx pgx.Tx, digest string, size int64, rank int) (blockformat.ObjectInspection, error) {
	var raw []byte
	// KEY SHARE prevents collection until the edge's restrictive FK takes over.
	err := tx.QueryRow(ctx, `SELECT inspection FROM computer_objects WHERE environment_id=$1 AND computer_id=$2 AND digest=$3 AND size_bytes=$4 AND rank=$5 AND certified FOR KEY SHARE`, s.environmentID, s.computerID, digest, size, rank).Scan(&raw)
	if err != nil {
		return blockformat.ObjectInspection{}, err
	}
	var evidence blockformat.ObjectInspection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&evidence)
	return evidence, err
}

func (s objectScope) insertEdge(ctx context.Context, tx pgx.Tx, parent inspectedObject, digest string, rank int) error {
	_, err := tx.Exec(ctx, `INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, s.environmentID, s.computerID, parent.digest, digest, parent.rank, rank)
	return err
}

func storageUnavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
}
