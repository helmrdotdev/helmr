package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeSessionDiscoveryRechecksCallerAfterLockWait(t *testing.T) {
	for _, operation := range []string{"get", "list"} {
		for _, change := range []string{"close", "output"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := newFixture(t)
				caller := runtimeReadCaller(t, f)
				target := f.peer(t)
				target.enqueue(t, "initial")
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, target.session)
				tx, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if _, err = lockSession(t.Context(), tx, f.env, f.session); err != nil {
					t.Fatal(err)
				}
				reader, err := f.pool.Acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					tx.Rollback(context.Background())
					reader.Release()
				}()
				result := make(chan error, 1)
				go func() {
					var err error
					if operation == "get" {
						_, err = RuntimeGetSession(t.Context(), reader, caller, target.session)
					} else {
						_, err = RuntimeListSessions(t.Context(), reader, caller, RuntimeSessionListRequest{Relation: "requested", Limit: 10})
					}
					result <- err
				}()
				deadline := time.Now().Add(3 * time.Second)
				for {
					var waiting bool
					if err = f.pool.QueryRow(t.Context(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, reader.Conn().PgConn().PID()).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("discovery did not wait on caller lock")
					}
					time.Sleep(time.Millisecond)
				}
				if change == "close" {
					err = closeProcessing(t.Context(), tx, f.execution(), caller.TurnID)
				} else {
					err = eventData(t.Context(), tx, f.env, f.session, caller.TurnID, "turn.output", json.RawMessage(`[{"type":"text","text":"still running"}]`))
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				err = <-result
				if change == "close" && !errors.Is(err, ErrDenied) {
					t.Fatalf("closed caller authorized after lock wait: %v", err)
				}
				if change == "output" && err != nil {
					t.Fatalf("live caller output caused discovery failure: %v", err)
				}
			})
		}
	}
}

func TestRuntimeSessionDiscoveryRetainsRelationsAndReceipts(t *testing.T) {
	f := newFixture(t)
	caller := runtimeReadCaller(t, f)
	owned, independent, unrelated := f.peer(t), f.peer(t), f.peer(t)
	for _, target := range []fixture{owned, independent} {
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET requester_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, target.session)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,root_session_id=$1 WHERE id=$2`, f.session, owned.session)
	ownedTurn := owned.enqueue(t, "owned")
	requestedTurn := independent.enqueue(t, "requested")
	unrelated.enqueue(t, "unrelated")
	// Sending a new Turn to an unrelated target permits exact receipt observation,
	// not discovery of its Session metadata or other work.
	if _, err := Enqueue(t.Context(), f.pool, caller, EnqueueRequest{EnvironmentID: f.env, SessionID: unrelated.session, RetryKey: "sent", Input: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeGetSession(t.Context(), f.pool, caller, unrelated.session); !errors.Is(err, ErrDenied) {
		t.Fatalf("unrelated Session: %v", err)
	}
	if _, err := RuntimeGetSession(t.Context(), f.pool, caller, uuid.NewV7()); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown Session: %v", err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: independent.session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	view, err := RuntimeGetSession(t.Context(), f.pool, caller, independent.session)
	if err != nil || view.Status != "cancelled" || view.InitialTurnID == nil || *view.InitialTurnID != requestedTurn.TurnID || view.InitialTurnStatus == nil || *view.InitialTurnStatus != "cancelled" || view.RequesterSessionID == nil || *view.RequesterSessionID != f.session {
		t.Fatalf("retained initial receipt: %+v %v", view, err)
	}
	page, err := RuntimeListSessions(t.Context(), f.pool, caller, RuntimeSessionListRequest{Relation: "owned", Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != owned.session || *page.Sessions[0].InitialTurnID != ownedTurn.TurnID {
		t.Fatalf("owned page: %+v %v", page, err)
	}
	seen := map[uuid.UUID]bool{}
	for before := uuid.Nil(); ; {
		page, err = RuntimeListSessions(t.Context(), f.pool, caller, RuntimeSessionListRequest{Relation: "requested", Before: before, Limit: 1})
		if err != nil || len(page.Sessions) != 1 || seen[page.Sessions[0].ID] {
			t.Fatalf("requested page: %+v %v", page, err)
		}
		seen[page.Sessions[0].ID] = true
		if page.NextCursor == uuid.Nil() {
			break
		}
		before = page.NextCursor
	}
	if len(seen) != 2 || !seen[owned.session] || !seen[independent.session] {
		t.Fatalf("requester scope: %v", seen)
	}
	page, err = RuntimeListSessions(t.Context(), f.pool, caller, RuntimeSessionListRequest{Relation: "requested", Statuses: []string{"cancelled"}, Limit: 100})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != independent.session {
		t.Fatalf("terminal filter: %+v %v", page, err)
	}
	for _, req := range []RuntimeSessionListRequest{{Relation: "all", Limit: 1}, {Relation: "owned", Limit: 0}, {Relation: "owned", Limit: 101}, {Relation: "requested", Limit: 1, Statuses: []string{"completed"}}} {
		if _, err := RuntimeListSessions(t.Context(), f.pool, caller, req); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid query %+v: %v", req, err)
		}
	}
	if err := CloseProcessing(t.Context(), f.pool, f.execution(), caller.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeGetSession(t.Context(), f.pool, caller, owned.session); !errors.Is(err, ErrDenied) {
		t.Fatalf("closed invoking Turn: %v", err)
	}
	// MCP carries Session authority without borrowing the current/latest Turn.
	caller.TurnID = uuid.Nil()
	if _, err := RuntimeGetSession(t.Context(), f.pool, caller, owned.session); err != nil {
		t.Fatalf("Session-bound discovery: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.session)
	if _, err := RuntimeListSessions(t.Context(), f.pool, caller, RuntimeSessionListRequest{Relation: "owned", Limit: 1}); !errors.Is(err, ErrDenied) {
		t.Fatalf("old authority: %v", err)
	}
}
