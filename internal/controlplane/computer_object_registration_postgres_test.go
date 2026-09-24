package controlplane

import (
	"bytes"
	"encoding/json"
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
	inspect := func(ref blockformat.PackRef) computerObjectInspection {
		t.Helper()
		p, err := blockformat.InspectPack(t.Context(), store, key.Scope, writer.Keys, ref)
		if err != nil {
			t.Fatal(err)
		}
		return computerObjectInspection{Pack: &p}
	}
	rootEvidence := inspect(root.Pack)
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, rootEvidence, false); err == nil {
		t.Fatal("uncertified child accepted")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_objects`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed registration leaked: %d %v", count, err)
	}
	upload := func(e computerObjectInspection) {
		t.Helper()
		o, err := describeComputerObject(e)
		if err != nil {
			t.Fatal(err)
		}
		// CAS membership is separately established by the existing upload verifier.
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream') ON CONFLICT DO NOTHING`, f.OrgID, o.digest, o.size)
	}
	registered := map[string]bool{}
	record := func(e computerObjectInspection) {
		t.Helper()
		o, err := describeComputerObject(e)
		if err != nil {
			t.Fatal(err)
		}
		if registered[o.digest] {
			return
		}
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("certification without registration succeeded")
		}
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, false); err != nil {
			t.Fatal(err)
		}
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, false); err != nil {
			t.Fatal(err)
		}
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("certification before upload succeeded")
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,'application/octet-stream')`, f.OrgID, o.digest, o.size+1)
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err == nil {
			t.Fatal("mismatched uploaded descriptor accepted")
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM cas_objects WHERE org_id=$1 AND digest=$2`, f.OrgID, o.digest)
		upload(e)
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err != nil {
			t.Fatal(err)
		}
		if err = recordInitialComputerObject(t.Context(), f.Pool, fence, e, true); err != nil {
			t.Fatal(err)
		}
		registered[o.digest] = true
	}
	var visit func(computerObjectInspection)
	visit = func(e computerObjectInspection) {
		for _, page := range e.Pack.Pages {
			for _, child := range page.Children {
				visit(inspect(child.Locator.Pack))
			}
			for _, segment := range page.Segments {
				if err = blockformat.InspectSegment(t.Context(), store, key.Scope, key.Key, segment); err != nil {
					t.Fatal(err)
				}
				record(computerObjectInspection{Segment: &segment})
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
	var changed computerObjectInspection
	if err = json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Pack.Pages[0].Locator.Offset++
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, changed, false); err == nil {
		t.Fatal("changed inspection accepted")
	}
	stale := fence
	stale.ClaimVersion++
	if err = recordInitialComputerObject(t.Context(), f.Pool, stale, rootEvidence, true); err == nil {
		t.Fatal("stale claims accepted")
	}
	stale = fence
	stale.DesiredVersion++
	if err = recordInitialComputerObject(t.Context(), f.Pool, stale, rootEvidence, false); err == nil {
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
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, bad, false); err == nil {
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
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, computerObjectInspection{Pack: &other}, false); err == nil {
		t.Fatal("unpinned write key accepted")
	}
}
