package workergroup

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func queuedTurn(t *testing.T, f agenttest.Fixture) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,request_digest,input)
 VALUES($1,$2,$3,$4,1,'user',$5,'send',$3,decode(repeat('00',32),'hex'),'null')`, f.Environment, uuid.NewV7(), f.Session, f.Computer, f.User)
}

func TestQueuedDemandRespectsRegionHoldsAndRevocation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
		want   bool
	}{
		{name: "queued", want: true},
		{name: "live process backlog", change: `UPDATE session_processes SET status='ready',fenced_at=NULL WHERE environment_id=$1 AND session_id=$2`},
		{name: "exhausted process epoch", change: `UPDATE session_processes SET epoch=9223372036854775807 WHERE environment_id=$1 AND session_id=$2`},
		{name: "local hold", change: `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$3,$2,'local','paused')`},
		{name: "released hold", change: `INSERT INTO session_holds(environment_id,id,session_id,scope,reason,released_at) VALUES($1,$3,$2,'local','paused',clock_timestamp())`, want: true},
		{name: "closed", change: `UPDATE sessions SET status='closed' WHERE environment_id=$1 AND id=$2`},
		{name: "revoked", change: `UPDATE deployments SET execution_revoked_at=clock_timestamp() WHERE environment_id=$1`},
		{name: "retired", change: `UPDATE environments SET retired_at=clock_timestamp() WHERE id=$1`},
		{name: "other region", change: `INSERT INTO regions(id,display_name) VALUES('elsewhere','Elsewhere'); UPDATE projects SET default_region_id='elsewhere' WHERE id=(SELECT project_id FROM environments WHERE id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := agenttest.New(t)
			queuedTurn(t, f)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.Environment, f.Session)
			if tc.change != "" {
				dbtest.MustExec(t, t.Context(), f.Pool, "SELECT $1::uuid,$2::uuid,$3::uuid; "+tc.change, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Session, uuid.NewV7())
			}
			got, err := HasQueuedDemand(t.Context(), f.Pool, f.Group)
			if err != nil || got != tc.want {
				t.Fatalf("queued demand=%v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestQueuedDemandIncludesUnassignedPreparationsUntilDeadline(t *testing.T) {
	f := agenttest.New(t)
	got, err := HasQueuedDemand(t.Context(), f.Pool, f.Group)
	if err != nil || got {
		t.Fatalf("idle fixture: %v %v", got, err)
	}
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,deadline_at) VALUES($1,$2,$3,'pending',clock_timestamp()+interval '1 hour')`, f.Environment, id, f.Deployment)
	got, err = HasQueuedDemand(t.Context(), f.Pool, f.Group)
	if err != nil || !got {
		t.Fatalf("queued preparation: %v %v", got, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.Environment, id)
	got, err = HasQueuedDemand(t.Context(), f.Pool, f.Group)
	if err != nil || got {
		t.Fatalf("expired preparation: %v %v", got, err)
	}
}
