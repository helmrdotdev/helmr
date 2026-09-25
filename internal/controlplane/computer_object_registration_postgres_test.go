package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestInitialComputerObjectInspectedRegistration(t *testing.T) {
	f, broker, fence := initialKeyFixture(t)
	key, err := broker.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := dispatch.LockComputerPreparation(t.Context(), tx, fence.ComputerPreparationFence)
	_ = tx.Rollback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capacity := owner.LogicalBytes
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
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, rootEvidence, false); err == nil {
		t.Fatal("uncertified child accepted")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration leaked: %d %v", count, err)
	}
	upload := func(e blockformat.ObjectInspection) {
		t.Helper()
		o, err := describeComputerObject(e)
		if err != nil {
			t.Fatal(err)
		}
		// CAS membership is separately established by the existing upload verifier.
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream') ON CONFLICT DO NOTHING`, f.OrgID, o.digest, o.size)
	}
	registered := map[string]bool{}
	record := func(e blockformat.ObjectInspection) {
		t.Helper()
		o, err := describeComputerObject(e)
		if err != nil {
			t.Fatal(err)
		}
		if registered[o.digest] {
			return
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("certification without registration succeeded")
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, false); err != nil {
			t.Fatal(err)
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, false); err != nil {
			t.Fatal(err)
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("certification before upload succeeded")
		}
		_, err = f.Pool.Exec(t.Context(), `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream')`, f.OrgID, o.digest, o.size+1)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23503" {
			t.Fatalf("conflicting blob size accepted: %v", err)
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("mismatched uploaded descriptor accepted")
		}
		upload(e)
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err != nil {
			t.Fatal(err)
		}
		if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err != nil {
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
	// changed declaration or stale Worker/Runtime authority.
	encoded, _ := json.Marshal(rootEvidence)
	var changed blockformat.ObjectInspection
	if err = json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Pack.Pages[0].Locator.Offset++
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, changed, false); err == nil {
		t.Fatal("changed inspection accepted")
	}
	stale := fence
	stale.ClaimVersion++
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, stale, rootEvidence, true); err == nil {
		t.Fatal("stale claims accepted")
	}
	stale = fence
	stale.DesiredVersion++
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, stale, rootEvidence, false); err == nil {
		t.Fatal("stale runtime accepted")
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
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, bad, false); err == nil {
		t.Fatal("wrong child position accepted")
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
	if err = testRecordInitialComputerObject(t.Context(), f.Pool, fence, blockformat.ObjectInspection{Pack: &other}, false); err == nil {
		t.Fatal("unpinned write key accepted")
	}
}

// The storage verification boundary is exercised by authenticated HTTP tests.
// This fixture selects only independently established CAS membership.
func testRecordInitialComputerObject(ctx context.Context, tx TxBeginner, f computerKeyFence, e blockformat.ObjectInspection, certify bool) error {
	if !certify {
		return recordInitialComputerObject(ctx, tx, f, e, nil)
	}
	object, err := describeComputerObject(e)
	if err != nil {
		return err
	}
	t, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer t.Rollback(context.WithoutCancel(ctx))
	owner, err := dispatch.LockComputerPreparation(ctx, t, f.ComputerPreparationFence)
	if err != nil {
		return err
	}
	uploaded, err := db.New(t).GetCasObject(ctx, db.GetCasObjectParams{OrgID: owner.OrgID, Digest: object.digest})
	if err != nil {
		return err
	}
	if err = t.Rollback(ctx); err != nil {
		return err
	}
	return recordInitialComputerObject(ctx, tx, f, e, &cas.Object{Digest: uploaded.Digest, SizeBytes: uploaded.SizeBytes, MediaType: uploaded.MediaType})
}
