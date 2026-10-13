package agent

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestDiskRootColumnsKeepIdentityAndObjectAuthority covers the typed disk root
// row: it rebuilds the exact root identity, retains an identical root
// idempotently, refuses a different root at the same pack position, and keeps
// the certified-object, direct-key and bounds guarantees in the schema.
func TestDiskRootColumnsKeepIdentityAndObjectAuthority(t *testing.T) {
	f := agenttest.New(t)
	ctx := t.Context()
	var rootID uuid.UUID
	var initial []byte
	var root disk.VersionRoot
	if err := f.Pool.QueryRow(ctx, `SELECT r.id,c.initial_root_digest,`+diskRootColumns+` FROM computers c JOIN disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, f.Environment, f.Computer).
		Scan(append([]any{&rootID, &initial}, diskRootFields(&root)...)...); err != nil {
		t.Fatal(err)
	}
	identity, err := root.Digest()
	if err != nil || identity != "sha256:"+hex.EncodeToString(initial) {
		t.Fatalf("rebuilt identity %s, stored %x: %v", identity, initial, err)
	}
	retain := func(r disk.VersionRoot) (uuid.UUID, error) {
		params, err := retainDiskRootParams(pgvalue.UUID(f.Environment), r)
		if err != nil {
			return uuid.Nil(), err
		}
		id, err := db.New(f.Pool).RetainDiskRoot(ctx, params)
		if err != nil {
			return uuid.Nil(), err
		}
		return pgvalue.MustUUIDValue(id), nil
	}
	if again, err := retain(root); err != nil || again != rootID {
		t.Fatalf("identical root retained as %s, want %s: %v", again, rootID, err)
	}
	changed := root
	changed.Page.Digest = "sha256:" + strings.Repeat("e", 64)
	if _, err := retain(changed); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different root at the same pack position: %v", err)
	}
	reject := func(name, code string, mutate func(*db.RetainDiskRootParams)) {
		t.Helper()
		params, err := retainDiskRootParams(pgvalue.UUID(f.Environment), root)
		if err != nil {
			t.Fatal(err)
		}
		// Another in-bounds offset in the same pack, so the conflict arm is not taken.
		if params.PageOffset > 8 {
			params.PageOffset = 8
		} else {
			params.PageOffset++
		}
		mutate(&params)
		_, err = db.New(f.Pool).RetainDiskRoot(ctx, params)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s: %v, want SQLSTATE %s", name, err, code)
		}
	}
	reject("pack size differs from the certified object", "23503", func(p *db.RetainDiskRootParams) { p.PackSizeBytes += 4096 })
	reject("key is not a direct key of the pack", "23503", func(p *db.RetainDiskRootParams) {
		p.PageKeyID = pgvalue.UUID(uuid.NewV7())
	})
	reject("rank differs from the certified object", "23503", func(p *db.RetainDiskRootParams) {
		if p.PackRank == 2 {
			p.PackRank = 3
		} else {
			p.PackRank = 2
		}
	})
	reject("page outside its pack", "23514", func(p *db.RetainDiskRootParams) {
		p.PageOffset = p.PackSizeBytes
	})
}
