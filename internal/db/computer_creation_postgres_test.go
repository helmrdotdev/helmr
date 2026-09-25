package db_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"time"
	"uuid"
)

func TestComputerCreationHasUnpublishedStableRoot(t *testing.T) {
	f := runtest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	work := f.AddRunLease(t, "assigned", time.Now())
	q := db.New(f.Pool)
	scheduleID := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO schedules (id,environment_id,task_declared_id,deployment_definition_id,deployment_id,cron_pattern,timezone,status,effective_from,next_fire_at)
VALUES ($1,$2,'test-task',$3,$4,'* * * * *','UTC','active',now(),now()+interval '1 minute')`, scheduleID, f.EnvironmentID, f.TaskDefinitionID, f.DeploymentID)
	for _, kind := range []string{"current deployment", "Run deployment", "schedule"} {
		t.Run(kind, func(t *testing.T) {
			computerID, rootID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
			var err error
			if kind == "current deployment" {
				_, err = q.CreateWorkspaceFromCurrentDeployment(t.Context(), db.CreateWorkspaceFromCurrentDeploymentParams{
					OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
					DeploymentDefinitionID: pgvalue.UUID(f.WorkspaceDefinitionID), SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			} else if kind == "Run deployment" {
				_, err = q.CreateWorkspaceFromRunDeployment(t.Context(), db.CreateWorkspaceFromRunDeploymentParams{
					EnvironmentID: pgvalue.UUID(f.EnvironmentID), RunID: pgvalue.UUID(work.RunID), SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			} else {
				_, err = q.CreateWorkspaceForScheduleFire(t.Context(), db.CreateWorkspaceForScheduleFireParams{
					EnvironmentID: pgvalue.UUID(f.EnvironmentID), ScheduleID: scheduleID, ExpectedGeneration: 1,
					SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			var valid bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT v.id=$2 AND v.status='initializing' AND v.parent_version_id IS NULL
AND v.publisher_runtime_instance_id IS NULL AND v.root_pack_digest IS NULL AND v.logical_bytes=0 AND v.published_at IS NULL
FROM computers w JOIN computer_versions v ON v.id=w.head_version_id WHERE w.id=$1`, computerID, rootID).Scan(&valid); err != nil || !valid {
				t.Fatalf("new root fabricated persistence: %v %v", valid, err)
			}
			_, err = f.Pool.Exec(t.Context(), `UPDATE computer_versions SET status='committed',published_at=now() WHERE id=$1`, rootID)
			var check *pgconn.PgError
			if !errors.As(err, &check) || check.Code != "23514" {
				t.Fatalf("root without disk committed: %v", err)
			}
		})
	}
}
