package schedule

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDBAdmitterDiagnosticCausesRollback(t *testing.T) {
	pool := openSchedulePostgres(t)
	for _, tc := range []struct {
		name, setup string
		code        ErrorCode
	}{
		{"wrong kind pin with matching task", `WITH actor AS (
    INSERT INTO deployment_definitions (id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest)
    SELECT $2,environment_id,deployment_id,'actor',declared_id,manifest_version,manifest,manifest_digest
    FROM deployment_definitions WHERE id=$1 RETURNING id
   ) UPDATE schedules SET deployment_definition_id=actor.id FROM actor WHERE deployment_definition_id=$1`, ErrorInvalidSchedule},
		{"task absent", `UPDATE deployment_definitions SET kind='actor' WHERE id=$1`, ErrorTaskNotFound},
		{"program wrong kind", `UPDATE artifacts SET kind='workspace_image' WHERE id=(SELECT program_artifact_id FROM deployments WHERE id=(SELECT deployment_id FROM deployment_definitions WHERE id=$1))`, ErrorProgramUnavailable},
		{"secret selection", `DELETE FROM schedule_secrets WHERE schedule_id=(SELECT id FROM schedules WHERE deployment_definition_id=$1)`, ErrorSecretSelectionMismatch},
		{"sandbox absent", `DELETE FROM deployment_definitions WHERE kind='sandbox' AND deployment_id=(SELECT deployment_id FROM deployment_definitions WHERE id=$1)`, ErrorSandboxNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, digest := seedScheduleAdmission(t, pool)
			if tc.code == ErrorInvalidSchedule {
				dbtest.MustExec(t, t.Context(), pool, tc.setup, value.DeploymentDefinitionID, pgvalue.UUID(uuid.NewV7()))
			} else {
				dbtest.MustExec(t, t.Context(), pool, tc.setup, value.DeploymentDefinitionID)
			}
			admitter, err := NewDBAdmitter(pool, fixedAuthority{digest: digest})
			if err != nil {
				t.Fatal(err)
			}
			admitter.now = func() time.Time { return value.NextFireAt.Time }
			err = admitter.AdmitSchedule(t.Context(), value)
			var admission *AdmissionError
			if !errors.As(err, &admission) || admission.Code != tc.code {
				t.Fatalf("admission=%v, want %s", err, tc.code)
			}
			assertScheduleAdmissionCounts(t, pool, value, 0, 0)
			assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})
		})
	}
}

func TestWorkerDiagnosticPersistence(t *testing.T) {
	pool := openSchedulePostgres(t)
	for _, tc := range []struct {
		name  string
		code  ErrorCode
		stale bool
	}{
		{"unknown canonical", "future_schedule_failure", false},
		{"malformed eligible", "INVALID CODE", false},
		{"malformed stale", "INVALID CODE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, _ := seedScheduleAdmission(t, pool)
			if tc.stale {
				dbtest.MustExec(t, t.Context(), pool, `UPDATE schedules SET generation=generation+1 WHERE id=$1`, value.ID)
			}
			snapshot := func() string {
				var s string
				if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(s)::text FROM schedules s WHERE id=$1`, value.ID).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := snapshot()
			worker, err := NewWorker(nil, db.New(pool), &workerAdmitter{err: &AdmissionError{Code: tc.code, Message: "future diagnosis"}})
			if err != nil {
				t.Fatal(err)
			}
			err = worker.process(t.Context(), value)
			if tc.name == "malformed eligible" {
				var pe *pgconn.PgError
				if !errors.As(err, &pe) || pe.Code != "23514" {
					t.Fatalf("error=%v, want CHECK rejection", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tc.code != "future_schedule_failure" {
				if after := snapshot(); after != before {
					t.Fatalf("rejected/no-op admission changed row: before=%s after=%s", before, after)
				}
			} else {
				after, err := db.New(pool).GetSchedule(t.Context(), db.GetScheduleParams{EnvironmentID: value.EnvironmentID, ID: value.ID})
				if err != nil {
					t.Fatal(err)
				}
				var failure struct {
					Code, Message string
					Details       map[string]any
				}
				if err := json.Unmarshal(after.LastFailure, &failure); err != nil {
					t.Fatal(err)
				}
				if after.State != "errored" || after.ClaimedBy.Valid || after.ClaimExpiresAt.Valid || after.RetryStep.Valid || after.RetryAfter.Valid || failure.Code != string(tc.code) || failure.Message != "future diagnosis" || failure.Details == nil {
					t.Fatalf("permanent state=%+v failure=%+v", after, failure)
				}
			}
			assertScheduleAdmissionCounts(t, pool, value, 0, 0)
			assertScheduleCursor(t, pool, value, value.NextFireAt.Time, time.Time{})
		})
	}
}
