package dbtest

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
	"uuid"
)

// InsertComputerGeneration supplies scoped graph authority for DB-only fixtures.
// Its synthetic bytes are not evidence of cryptographic verification or VM I/O.
func InsertComputerGeneration(t *testing.T, ctx context.Context, tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, environment, computerID, version any) {
	t.Helper()
	key := uuid.NewV7().String()
	digest := Digest(key)
	root := computer.GenerationRoot{FormatVersion: 1, LogicalBytes: computer.SeedCapacity, Offset: 128,
		Pack: computer.GenerationPack{Digest: digest, SizeBytes: 512, Rank: 2},
		Page: computer.GenerationPage{Digest: Digest(key + "page"), Salt: strings.Repeat("aa", 32), KeyID: key, Kind: 3, Count: 1, SizeBytes: 64}}
	locator, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	MustExec(t, ctx, tx, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, environment, computerID)
	MustExec(t, ctx, tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,512)`, digest)
	MustExec(t, ctx, tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,512,'application/octet-stream' FROM environments WHERE id=$1`, environment, digest)
	MustExec(t, ctx, tx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$2,$3,org_id,project_id,512,'application/octet-stream','root',2,'{}',now() FROM environments WHERE id=$1`, environment, computerID, digest)
	MustExec(t, ctx, tx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, environment, computerID, digest, key)
	MustExec(t, ctx, tx, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator) VALUES($1,$2,$3,$4)`, environment, computerID, version, locator)
}
