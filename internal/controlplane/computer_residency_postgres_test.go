package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerResidencyProjectsCurrentInstance(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`1`), true)
	q := db.New(f.Pool)
	read := func(want string) {
		t.Helper()
		row, err := q.GetComputer(t.Context(), db.GetComputerParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(f.computerID)})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := computer.Read(t.Context(), q, computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID}, f.computerID)
		if err != nil {
			t.Fatal(err)
		}
		if string(snapshot.Residency) != want || snapshot.Status != "available" {
			t.Fatalf("projection=%+v want residency %s", snapshot, want)
		}
		if want == "unavailable" && len(snapshot.Error) == 0 {
			t.Fatal("unavailable without structured reason")
		}
		rows, err := q.ListComputerListItems(t.Context(), db.ListComputerListItemsParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), RowLimit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range rows {
			if item.ID == row.ID && item.Residency != want {
				t.Fatalf("list residency=%s", item.Residency)
			}
		}
	}
	read("running")
	for _, state := range []string{"draining", "checkpointing", "closed"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE computer_id=$1 AND reclaimed_at IS NULL`, f.computerID, state)
		read("parking")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='open' WHERE computer_id=$1 AND reclaimed_at IS NULL`, f.computerID)
	read("running")
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped',preparation_attempt_count=8,preparation_instance_id=(SELECT id FROM computer_instances WHERE computer_id=computers.id AND reclaimed_at IS NULL),preparation_failure='{"code":"computer_preparation_exhausted","message":"Preparation limit reached"}' WHERE id=$1`, f.computerID)
	read("unavailable")
}

func TestManagedParkingAdmitsLogicalWork(t *testing.T) {
	for _, state := range []string{"draining", "checkpointing", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := newActorExecutionFixture(t, json.RawMessage(`1`), true)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state=$2 WHERE computer_id=$1 AND reclaimed_at IS NULL`, f.computerID, state)
			command, err := f.server.admitComputerCommand(t.Context(), computerCommandRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, ComputerID: f.computerID, Creator: computerCommandCreator{SubjectType: "api_key", SubjectID: f.runID.String()}, Command: []string{"true"}, IdempotencyKey: "during-parking"})
			if err != nil {
				t.Fatal(err)
			}
			if command.Process.Status != "pending" || command.Process.ComputerInstanceID.Valid {
				t.Fatalf("command crossed physical barrier: %+v", command.Process)
			}
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			q := db.New(tx)
			locked, err := q.LockComputer(t.Context(), db.LockComputerParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(f.computerID)})
			if err != nil {
				t.Fatal(err)
			}
			var deployment pgtype.UUID
			if err := tx.QueryRow(t.Context(), `SELECT deployment_id FROM runs WHERE id=$1`, f.runID).Scan(&deployment); err != nil {
				t.Fatal(err)
			}
			ok, err := computer.CanAdmitProgram(t.Context(), q, locked.EnvironmentID, locked.ID, locked.ComputerSpecID, deployment)
			if err != nil || !ok {
				t.Fatalf("program admission during %s: %v %v", state, ok, err)
			}
		})
	}
}
