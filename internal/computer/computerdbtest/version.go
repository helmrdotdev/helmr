package computerdbtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5/pgconn"
)

// InsertComputerVersion supplies scoped graph authority for DB-only fixtures.
// Its synthetic bytes are not evidence of cryptographic verification or VM I/O.
func InsertComputerVersion(t *testing.T, ctx context.Context, tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, environment, computerID, version any) {
	t.Helper()
	key := uuid.NewV7().String()
	digest := dbtest.Digest(key)
	root := disk.VersionRoot{FormatVersion: 1, LogicalBytes: disk.SeedCapacity, Offset: 128,
		Pack: disk.VersionPack{Digest: digest, SizeBytes: 512, Rank: 2},
		Page: disk.VersionPage{Digest: dbtest.Digest(key + "page"), Salt: strings.Repeat("aa", 32), KeyID: key, Kind: 3, Count: 1, SizeBytes: 64}}
	locator, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, environment, computerID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,512)`, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,512,'application/octet-stream' FROM environments WHERE id=$1`, environment, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$2,org_id,project_id,512,'application/octet-stream','root',2,'{}',now()  FROM environments WHERE id=$1`, environment, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_object_keys(environment_id,digest,key_id,is_direct) VALUES($1,$2,$3,true)`, environment, digest, key)
	dbtest.MustExec(t, ctx, tx, `WITH root AS (INSERT INTO computer_disk_roots(id,environment_id,locator) VALUES(gen_random_uuid(),$1,$4) RETURNING id) INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,root_id) SELECT $1,$2,$3,id FROM root`, environment, computerID, version, locator)
}
