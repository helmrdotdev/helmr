package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// This supplies certified database state for placement tests, not proof of pack
// authentication or publication. Those boundaries are tested by the publisher.
func insertPlacementGeneration(t *testing.T, ctx context.Context, tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, environment, computerID, version any) {
	t.Helper()
	key := uuid.NewV7().String()
	digest := dbtest.Digest(key)
	root := computer.GenerationRoot{FormatVersion: 1, LogicalBytes: int64(compute.WorkspaceGuestEphemeralDiskMiB) * 1048576, Offset: 128,
		Pack: computer.GenerationPack{Digest: digest, SizeBytes: 512, Rank: 2},
		Page: computer.GenerationPage{Digest: dbtest.Digest(key + "page"), Salt: strings.Repeat("aa", 32), KeyID: key, Kind: 3, Count: 1, SizeBytes: 64}}
	locator, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, key, environment, computerID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_object_lifetimes(digest) VALUES($1)`, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,512,'application/octet-stream' FROM environments WHERE id=$1`, environment, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$2,$3,org_id,project_id,512,'application/octet-stream','root',2,'{}',now() FROM environments WHERE id=$1`, environment, computerID, digest)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, environment, computerID, digest, key)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,locator) VALUES($1,$2,$3,$4)`, environment, computerID, version, locator)
}

func TestRuntimeReservationRetainsExactComputerGeneration(t *testing.T) {
	for _, kind := range []string{"run", "exec"} {
		for _, state := range []string{"committed", "initializing", "missing root", "wrong capacity"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				f := newRunPlacementFixture(t)
				var version pgtype.UUID
				if err := f.pool.QueryRow(f.ctx, `SELECT head_version_id FROM computers WHERE id=$1`, f.workspaceID).Scan(&version); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "initializing":
					dbtest.MustExec(t, f.ctx, f.pool, `DELETE FROM computer_version_roots WHERE version_id=$1`, version)
					dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_versions SET status='initializing',artifact_id=NULL,content_digest=NULL,size_bytes=0,published_at=NULL WHERE id=$1`, version)
				case "missing root":
					dbtest.MustExec(t, f.ctx, f.pool, `DELETE FROM computer_version_roots WHERE version_id=$1`, version)
				case "wrong capacity":
					dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_version_roots SET locator=jsonb_set(locator,'{logical_bytes}','4096') WHERE version_id=$1`, version)
				}
				var runtime pgtype.UUID
				var err error
				if kind == "run" {
					result, e := f.authority.PlaceReadyRun(f.ctx, f.candidate())
					runtime, err = result.RuntimeInstanceID, e
				} else {
					process := createPendingWorkspaceExec(t, f)
					result, e := f.authority.PlaceWorkspaceExec(f.ctx, ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(process), ExpectedRevision: 1})
					runtime, err = result.RuntimeInstanceID, e
				}
				if state == "missing root" || state == "wrong capacity" {
					if err == nil {
						t.Fatal("invalid source allocated")
					}
					var count int
					if e := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runtime_instances WHERE workspace_id=$1`, f.workspaceID).Scan(&count); e != nil || count != 0 {
						t.Fatalf("partial allocation: %d %v", count, e)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var source, retained pgtype.UUID
				if err = f.pool.QueryRow(f.ctx, `SELECT computer_source_version_id,retained_computer_source_version_id FROM runtime_instances WHERE id=$1`, runtime).Scan(&source, &retained); err != nil {
					t.Fatal(err)
				}
				if state == "initializing" {
					if source.Valid || retained.Valid {
						t.Fatal("unpublished source pinned")
					}
					return
				}
				if source != version || retained != version {
					t.Fatalf("wrong source: %v %v", source, retained)
				}
				_, err = f.pool.Exec(f.ctx, `DELETE FROM computer_version_roots WHERE version_id=$1`, version)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || (pgerr.Code != "23503" && pgerr.Code != "23001") {
					t.Fatalf("source not retained: %v", err)
				}
			})
		}
	}
}
