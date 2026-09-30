package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerComputerMembersRequiresLiveRunAuthority(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	expiresAt := time.Now().Add(10 * time.Minute).UTC()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_leases
   SET status = 'running', started_at = now(), expires_at = $2
 WHERE id = $1`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE computer_instances SET writer_expires_at = $2
 WHERE id = (SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runs
   SET status = 'running', started_at = now(), active_started_at = now()
 WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE run_attempts SET entrypoint_entered_at = now()
 WHERE run_id = $1 AND number = 1`, work.RunID)
	var workerClaimVersion, groupClaimVersion int64
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_hosts.claim_version, worker_groups.claim_version
  FROM worker_hosts
  JOIN worker_groups ON worker_groups.id = worker_hosts.worker_group_id
 WHERE worker_hosts.id = $1`, fixture.WorkerID).Scan(
		&workerClaimVersion, &groupClaimVersion,
	); err != nil {
		t.Fatal(err)
	}
	worker := workergroup.HostPrincipal{
		HostID: fixture.WorkerID, GroupID: runtest.WorkerGroupID,
		Epoch: 1, HostClaimVersion: workerClaimVersion, GroupClaimVersion: groupClaimVersion,
	}
	var computerID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	request := workerapi.ComputerMembersRequest{RetrieveComputerRequest: workerapi.RetrieveComputerRequest{
		Lease:         workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1},
		CorrelationID: uuid.NewV7().String(), Computer: workerapi.ComputerAddress{ComputerID: computerID.String()},
	}}
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	invoke := func(want int) workerapi.ComputerMembersResponse {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/worker/v1/run/computers/members", bytes.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), workerContextKey{}, worker))
		w := httptest.NewRecorder()
		server.workerListComputerMembers(w, r)
		if w.Code != want {
			t.Fatalf("members status=%d want=%d: %s", w.Code, want, w.Body.String())
		}
		var response workerapi.ComputerMembersResponse
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return response
	}
	page := invoke(http.StatusOK)
	if page.Completed == nil || len(page.Completed.Members) != 1 || page.Completed.Members[0].Kind != "task" || page.Completed.Members[0].ID != work.RunID.String() || page.Completed.Members[0].RunID != work.RunID.String() || page.Completed.Members[0].State != "running" {
		t.Fatalf("members=%+v", page)
	}
	request.Cursor = "invalid"
	invalid := invoke(http.StatusOK)
	if invalid.Completed != nil || invalid.Failed == nil || invalid.Failed.Code != "invalid_computer_reference" {
		t.Fatalf("invalid cursor=%+v", invalid)
	}
	request.Cursor = ""
	request.Computer.ComputerID = uuid.NewV7().String()
	absent := invoke(http.StatusOK)
	if absent.Failed == nil || absent.Failed.Code != "computer_not_found" {
		t.Fatalf("unknown Computer=%+v", absent)
	}
	request.Computer.ComputerID = computerID.String()
	request.Lease.LeaseSequence = 2
	invoke(http.StatusConflict)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE runs SET status='queued',current_run_lease_id=NULL WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO run_waits
 (id,environment_id,run_id,computer_id,kind,condition_status,condition_result,condition_terminal_at,
 due_at,suspension_status,expected_run_revision,attempt_number,prior_run_lease_id)
 VALUES ($1,$2,$3,$4,'timer','completed','null',now(),now(),'resume_pending',1,1,$5)`,
		uuid.NewV7(), fixture.EnvironmentID, work.RunID, computerID, work.LeaseID)
	members, err := server.db.ListComputerMembers(t.Context(), db.ListComputerMembersParams{
		EnvironmentID: pgvalue.UUID(fixture.EnvironmentID), ComputerID: pgvalue.UUID(computerID), RowLimit: 100,
	})
	if err != nil || len(members) != 1 || members[0].State != "parked" || members[0].RunID != pgvalue.UUID(work.RunID) {
		t.Fatalf("queued Task waiting for restore=%+v, %v", members, err)
	}
}
