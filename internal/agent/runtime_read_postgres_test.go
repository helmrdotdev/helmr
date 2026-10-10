package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func runtimeReadCaller(t *testing.T, f fixture) Caller {
	t.Helper()
	a := f.enqueue(t, "caller")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	return Caller{Kind: "session", ID: f.session, TurnID: a.TurnID, Execution: f.execution(), Host: f.host()}
}

func TestRuntimeTurnObservationScope(t *testing.T) {
	f := newFixture(t)
	caller := runtimeReadCaller(t, f)
	peer := f.peer(t)
	unrelated := peer.enqueue(t, "unrelated")
	own, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "own", Input: json.RawMessage(`[{"type":"text","text":"{\"private\":\"input\"}"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, own.TurnID)
	if err != nil || view.Status != "queued" || view.Input != nil || view.ID != own.TurnID {
		t.Fatalf("own receipt: %+v %v", view, err)
	}
	if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, unrelated.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("unrelated: %v", err)
	}
	if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, f.session, own.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong Session: %v", err)
	}
	other := newFixture(t)
	foreign := other.enqueue(t, "foreign")
	if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, other.session, foreign.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign Environment: %v", err)
	}
	// Session-bound MCP has no inferred source Turn, but retains exact admission attribution.
	caller.TurnID = uuid.Nil()
	if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, own.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, unrelated.TurnID); !errors.Is(err, ErrDenied) {
		t.Fatalf("MCP unrelated: %v", err)
	}
}

func TestRuntimeTurnObservationCreatedWorkAndFinalizing(t *testing.T) {
	for _, relationship := range []string{"owned", "requested"} {
		t.Run(relationship, func(t *testing.T) {
			f := newFixture(t)
			caller := runtimeReadCaller(t, f)
			peer := f.peer(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, peer.session)
			if relationship == "owned" {
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,root_session_id=$1 WHERE id=$2`, f.session, peer.session)
			}
			storage := newSaveStorageFixture(t, peer)
			a, save := peer.finalize(t, "result")
			view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, a.TurnID)
			if err != nil || view.Status != "finalizing" || view.Result != nil || view.Response != nil {
				t.Fatalf("provisional: %+v %v", view, err)
			}
			// The normal publication path settles the Turn only after its own save.
			root, _ := storage.cut(t, 8)
			if err := storage.publisher.Capture(t.Context(), storage.ref(save.ID), root, "owned cut"); err != nil {
				t.Fatal(err)
			}
			if err := storage.publish(t, save.ID, root); err != nil {
				t.Fatal(err)
			}
			if err := Complete(t.Context(), f.pool, f.env, peer.session, a.TurnID); err != nil {
				t.Fatal(err)
			}
			view, err = RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, a.TurnID)
			var result map[string]int
			decodeErr := json.Unmarshal(view.Result, &result)
			if err != nil || view.Status != "completed" || decodeErr != nil || result["result"] != 1 {
				t.Fatalf("settled: %+v %v", view, err)
			}
			session, err := RuntimeGetSession(t.Context(), f.pool, caller, peer.session)
			if err != nil || session.InitialTurnID == nil || *session.InitialTurnID != a.TurnID || session.InitialTurnStatus == nil || *session.InitialTurnStatus != "completed" {
				t.Fatalf("completed discovery: %+v %v", session, err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET input=NULL,result=NULL,response=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, a.TurnID)
			view, err = RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, a.TurnID)
			if err != nil || view.Status != "completed" || view.PayloadExpiredAt == nil || view.Result != nil {
				t.Fatalf("expired: %+v %v", view, err)
			}
		})
	}
}

func TestRuntimeTurnObservationRechecksCaller(t *testing.T) {
	for _, change := range []string{"generation", "lease", "host", "hold", "processing", "deadline", "deployment"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			caller := runtimeReadCaller(t, f)
			peer := f.peer(t)
			a, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: peer.session, RetryKey: "own", Input: json.RawMessage(`[]`)})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "generation":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.session)
			case "lease":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
			case "host":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET current_epoch=2`)
			case "hold":
				dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO session_holds(environment_id,id,session_id,scope,reason) VALUES($1,$2,$3,'local','test')`, f.env, uuid.NewV7(), f.session)
			case "processing":
				if err := CloseProcessing(t.Context(), f.pool, f.execution(), caller.TurnID); err != nil {
					t.Fatal(err)
				}
			case "deadline":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE turns SET deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, caller.TurnID)
			case "deployment":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET execution_revoked_at=clock_timestamp()`)
			}
			if _, err := RuntimeObserveTurn(t.Context(), f.pool, caller, peer.session, a.TurnID); !errors.Is(err, ErrDenied) {
				t.Fatalf("stale caller: %v", err)
			}
		})
	}
}

func TestRuntimeTurnWaitReportsOnlyKnownRetainedCapacityDependency(t *testing.T) {
	for _, limit := range []string{"resident", "cpu", "memory"} {
		t.Run(limit, func(t *testing.T) {
			f := newAdmissionFixture(t)
			caller := runtimeReadCaller(t, f)
			// Fractional declared CPU still reserves a whole physical vCPU.
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_definitions SET resources='{"milliCpu":500,"memoryMiB":512}' WHERE environment_id=$1`, f.env)
			receipt, err := Start(t.Context(), f.pool, nil, caller, f.startRequest("target"))
			if err != nil {
				t.Fatal(err)
			}
			if view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID); err != nil || view.WaitBlocked != "" {
				t.Fatalf("ordinary pending %+v %v", view, err)
			}
			switch limit {
			case "resident":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_resident_computers=1 WHERE id=$1`, f.env)
			case "cpu":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_cpu_millis=1999 WHERE id=$1`, f.env)
			case "memory":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_memory_bytes=1073741823 WHERE id=$1`, f.env)
			}
			view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID)
			if err != nil || view.Status != "queued" || view.WaitBlocked != "capacity_wait_blocked" {
				t.Fatalf("missing dependency %+v %v", view, err)
			}
			if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: receipt.SessionID, Kind: "interrupt", RetryKey: "held-target"}); err != nil {
				t.Fatal(err)
			}
			view, err = RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID)
			if err != nil || view.Status != "queued" || view.WaitBlocked != "" {
				t.Fatalf("held target misclassified %+v %v", view, err)
			}
			if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: receipt.SessionID, Kind: "cancel", RetryKey: "cancel-target"}); err != nil {
				t.Fatal(err)
			}
			view, err = RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID)
			if err != nil || view.Status != "cancelled" || view.WaitBlocked != "" {
				t.Fatalf("terminal lost to capacity %+v %v", view, err)
			}
		})
	}
	t.Run("shared Computer requires no extra allocation", func(t *testing.T) {
		f := newAdmissionFixture(t)
		caller := runtimeReadCaller(t, f)
		request := f.startRequest("shared")
		request.ComputerID = f.computer
		receipt, err := Start(t.Context(), f.pool, nil, caller, request)
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE environments SET max_resident_computers=1,max_cpu_millis=1000,max_memory_bytes=536870912 WHERE id=$1`, f.env)
		view, err := RuntimeObserveTurn(t.Context(), f.pool, caller, receipt.SessionID, receipt.TurnID)
		if err != nil || view.WaitBlocked != "" {
			t.Fatalf("shared allocation %+v %v", view, err)
		}
	})
}
