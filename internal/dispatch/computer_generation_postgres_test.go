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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
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
	dbtest.MustExec(t, ctx, tx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,512)`, digest)
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

func TestExecPlacementUsesGenerationWithoutArtifact(t *testing.T) {
	f := newRunPlacementFixture(t)
	root := workspaceHeadVersion(t, f)
	process := createPendingWorkspaceExec(t, f)
	candidate := ReadyWorkspaceExecCandidate{OrgID: pgvalue.UUID(f.orgID), ProcessID: pgvalue.UUID(process), ExpectedRevision: 1}
	reserved, err := f.authority.PlaceWorkspaceExec(f.ctx, candidate)
	if err != nil || !reserved.RuntimeInstanceID.Valid {
		t.Fatalf("reserve: %+v %v", reserved, err)
	}
	// Model initial generation publication by the reserved Runtime, without a
	// whole-file artifact. Publication itself has separate HTTP/byte-level tests.
	dbtest.MustExec(t, f.ctx, f.pool, `UPDATE computer_versions SET artifact_id=NULL,publisher_runtime_instance_id=$2,publisher_desired_version=1,publication_request_fingerprint=decode(repeat('a1',32),'hex') WHERE id=$1`, root, reserved.RuntimeInstanceID)
	markRunPlacementRuntimeReady(t, f, reserved.RuntimeInstanceID)
	mounting, err := f.authority.PlaceWorkspaceExec(f.ctx, candidate)
	if err != nil || !mounting.WorkspaceMountID.Valid {
		t.Fatalf("mount: %+v %v", mounting, err)
	}
	markRunPlacementMountReady(t, f, mounting.WorkspaceMountID)
	bound, err := f.authority.PlaceWorkspaceExec(f.ctx, candidate)
	if err != nil || !bound.ProcessBound {
		t.Fatalf("grant: %+v %v", bound, err)
	}
	var origin pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT base_workspace_version_id FROM workspace_processes WHERE id=$1 AND workspace_mount_id=$2 AND status='starting'`, process, bound.WorkspaceMountID).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin != root {
		t.Fatal("execution source changed")
	}
}

func TestExecWriterRejectsArtifactWithoutGeneration(t *testing.T) {
	f := newRunPlacementFixture(t)
	root := workspaceHeadVersion(t, f)
	createPendingWorkspaceExec(t, f)
	dbtest.MustExec(t, f.ctx, f.pool, `DELETE FROM computer_version_roots WHERE version_id=$1`, root)
	var ownership, writer int64
	if err := f.pool.QueryRow(f.ctx, `SELECT ownership_generation,writer_generation FROM computers WHERE id=$1`, f.workspaceID).Scan(&ownership, &writer); err != nil {
		t.Fatal(err)
	}
	_, err := db.New(f.pool).AdvanceWorkspaceExecWriter(f.ctx, db.AdvanceWorkspaceExecWriterParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID), EnvironmentID: pgvalue.UUID(f.environmentID), WorkspaceID: pgvalue.UUID(f.workspaceID), BaseWorkspaceVersionID: root,
		ExpectedOwnershipGeneration: ownership, ExpectedWriterGeneration: writer, OwnershipGeneration: ownership + 1, WriterGeneration: writer + 1})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing generation granted writer: %v", err)
	}
}
