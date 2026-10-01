package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInitialComputerObjectInspectedRegistration(t *testing.T) {
	f := newPreparationFixture(t)
	key := f.initialKey(t)
	defer clear(key.Key)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capacity := f.logicalBytes
	writer := blockformat.Writer{Source: store, Sink: store, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	root, err := writer.Empty(t.Context(), capacity, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err = writer.Capture(t.Context(), root, capacity, map[uint64][]byte{0: bytes.Repeat([]byte{3}, 4096), 64: bytes.Repeat([]byte{4}, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	inspect := func(ref blockformat.PackRef) blockformat.ObjectInspection {
		t.Helper()
		p, err := blockformat.InspectPack(t.Context(), store, key.Scope, writer.Keys, ref)
		if err != nil {
			t.Fatal(err)
		}
		return blockformat.ObjectInspection{Pack: &p}
	}
	rootEvidence := inspect(root.Pack)
	if err = f.recordInitialObject(t.Context(), f.principal, f.ref, rootEvidence, false); err == nil {
		t.Fatal("uncertified child accepted")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration leaked: %d %v", count, err)
	}
	upload := func(e blockformat.ObjectInspection) {
		t.Helper()
		o, err := describeObject(e)
		if err != nil {
			t.Fatal(err)
		}
		// CAS membership is separately established by the existing upload verifier.
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream') ON CONFLICT DO NOTHING`, f.OrgID, o.digest, o.size)
	}
	registered := map[string]bool{}
	record := func(e blockformat.ObjectInspection) {
		t.Helper()
		o, err := describeObject(e)
		if err != nil {
			t.Fatal(err)
		}
		if registered[o.digest] {
			return
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, true); err == nil {
			t.Fatal("certification without registration succeeded")
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, false); err != nil {
			t.Fatal(err)
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, false); err != nil {
			t.Fatal(err)
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, true); err == nil {
			t.Fatal("certification before upload succeeded")
		}
		_, err = f.Pool.Exec(t.Context(), `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream')`, f.OrgID, o.digest, o.size+1)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23503" {
			t.Fatalf("conflicting blob size accepted: %v", err)
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, true); err == nil {
			t.Fatal("mismatched uploaded descriptor accepted")
		}
		upload(e)
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, true); err != nil {
			t.Fatal(err)
		}
		if err = f.recordInitialObject(t.Context(), f.principal, f.ref, e, true); err != nil {
			t.Fatal(err)
		}
		registered[o.digest] = true
	}
	var visit func(blockformat.ObjectInspection)
	visit = func(e blockformat.ObjectInspection) {
		for _, page := range e.Pack.Pages {
			for _, child := range page.Children {
				visit(inspect(child.Locator.Pack))
			}
			for _, segment := range page.Segments {
				if err = blockformat.InspectSegment(t.Context(), store, key.Scope, key.Key, segment); err != nil {
					t.Fatal(err)
				}
				record(blockformat.ObjectInspection{Segment: &segment})
			}
		}
		record(e)
	}
	visit(rootEvidence)
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects WHERE certified`).Scan(&count); err != nil || count != len(registered) {
		t.Fatalf("certified closure: %d %v", count, err)
	}
	// Reuse is exact even after certification. A stored receipt never authorizes a
	// changed declaration or stale Worker/Instance authority.
	encoded, _ := json.Marshal(rootEvidence)
	var changed blockformat.ObjectInspection
	if err = json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Pack.Pages[0].Locator.Offset++
	if err = f.recordInitialObject(t.Context(), f.principal, f.ref, changed, false); err == nil {
		t.Fatal("changed inspection accepted")
	}
	stale := f.principal
	stale.HostClaimVersion++
	if err = f.recordInitialObject(t.Context(), stale, f.ref, rootEvidence, true); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims accepted: %v", err)
	}
	staleRef := f.ref
	staleRef.DesiredVersion++
	if err = f.recordInitialObject(t.Context(), f.principal, staleRef, rootEvidence, false); err == nil {
		t.Fatal("stale instance accepted")
	}
	// A new parent referring to a real child at the wrong node position must not
	// reuse that child's digest as its entire proof.
	bad := rootEvidence
	copied := *bad.Pack
	copied.Pages = append([]blockformat.PageInspection(nil), bad.Pack.Pages...)
	bad.Pack = &copied
	copied.Pages[0].Locator.Pack.Digest[0] ^= 1
	copied.Pages[0].Children = append([]blockformat.NodeReference(nil), copied.Pages[0].Children...)
	copied.Pages[0].Children[0].Start++
	var childConflict ObjectConflictError
	if err = f.recordInitialObject(t.Context(), f.principal, f.ref, bad, false); !errors.As(err, &childConflict) {
		t.Fatalf("wrong child position = %v", err)
	}
	// Registered graph edges retain uploaded children.
	var childDigest string
	if err = f.Pool.QueryRow(t.Context(), `SELECT child_digest FROM computer_object_edges LIMIT 1`).Scan(&childDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Pool.Exec(t.Context(), `DELETE FROM computer_objects WHERE digest=$1`, childDigest); err == nil {
		t.Fatal("referenced child collected")
	}
	// Initialization cannot introduce a different direct write key.
	other := *rootEvidence.Pack
	other.Pages = append([]blockformat.PageInspection(nil), other.Pages...)
	other.Pages[0].Locator.Page.Key = pgvalue.UUIDString(pgvalue.NewUUIDv7())
	if err = f.recordInitialObject(t.Context(), f.principal, f.ref, blockformat.ObjectInspection{Pack: &other}, false); err == nil {
		t.Fatal("unpinned write key accepted")
	}
}

// recordInitialObject registers the object, or certifies it against the
// organization's CAS membership. The storage verification boundary is
// exercised by CertifyInitialObject and the authenticated HTTP tests; this
// helper selects only independently established CAS membership.
func (f preparationFixture) recordInitialObject(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, e blockformat.ObjectInspection, certify bool) error {
	if !certify {
		return f.publisher.RegisterInitialObject(ctx, principal, ref, e)
	}
	object, err := describeObject(e)
	if err != nil {
		return err
	}
	var uploaded db.CasObject
	if err = db.RunTx(ctx, f.Pool, func(tx pgx.Tx) error {
		fence, err := lockInitialFence(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		uploaded, err = db.New(tx).GetCasObject(ctx, db.GetCasObjectParams{OrgID: fence.orgID, Digest: object.digest})
		return err
	}); err != nil {
		return err
	}
	return f.publisher.inInitialPreparation(ctx, principal, ref, func(tx pgx.Tx, scope objectScope) error {
		return scope.certifyObject(ctx, tx, e, cas.Object{Digest: uploaded.Digest, SizeBytes: uploaded.SizeBytes, MediaType: uploaded.MediaType})
	})
}
