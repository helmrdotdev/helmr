package session_test

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"
)

func TestRuntimeMCPScopeDenialPreservesTargetNondisclosure(t *testing.T) {
	f := newFixture(t)
	hiddenAgent, helperAgent := uuid.NewV7(), uuid.NewV7()
	for key, id := range map[string]uuid.UUID{"hidden": hiddenAgent, "helper": helperAgent} {
		dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,$3); INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init,runtime_mcp_policy) SELECT environment_id,$2,deployment_id,template_id,has_init,runtime_mcp_policy FROM agent_definitions WHERE environment_id=$1 AND agent_id=$4`, pgx.QueryExecModeSimpleProtocol, f.env, id, key, f.agent)
	}
	sessiontest.PublishRuntimeAgentSet(t, sessiontest.Fixture{Pool: f.pool, Environment: f.env, Deployment: f.deployment}, "agent", "helper", "hidden")
	invisible := f.peer(t)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET agent_id=$3 WHERE environment_id=$1 AND id=$2`, f.env, invisible.session, hiddenAgent)
	hiddenTurn := invisible.enqueue(t, "hidden-human-turn")
	callerHumanTurn := f.enqueue(t, "caller-human-turn")
	rules := []map[string]any{}
	for _, op := range []string{"enqueue", "send_turn", "interrupt_turn", "interrupt_session", "resume_session", "close_session", "cancel_session", "inspect_session", "inspect_turn"} {
		rules = append(rules, map[string]any{"operation": op, "agents": []string{"helper"}})
	}
	raw, _ := json.Marshal(map[string]any{"allow": rules})
	caller := issueScope(t, f, string(raw))
	policy := auditpolicy.Policy{RetentionSeconds: 3600}
	operations := map[string]func(uuid.UUID, uuid.UUID) error{
		"inspect_session": func(target, turn uuid.UUID) error {
			_, err := session.RuntimeGetSession(t.Context(), policy, f.pool, caller, target)
			return err
		},
		"inspect_turn": func(target, turn uuid.UUID) error {
			_, err := session.RuntimeObserveTurn(t.Context(), policy, f.pool, caller, target, turn)
			return err
		},
		"enqueue": func(target, turn uuid.UUID) error {
			_, err := session.Enqueue(t.Context(), policy, f.pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.env, SessionID: target, IdempotencyKey: "nondisclosure-enqueue", Input: json.RawMessage(`[]`)})
			return err
		},
		"send_turn": func(target, turn uuid.UUID) error {
			_, err := session.SendTurn(t.Context(), policy, f.pool, caller, session.EnqueueRequest{EnvironmentID: f.env, SessionID: target, IdempotencyKey: "nondisclosure-send", Input: json.RawMessage(`[{"type":"text","text":"hello"}]`)}, turn)
			return err
		},
	}
	for _, kind := range []string{"interrupt", "resume", "close", "cancel", "turn_interrupt"} {
		operations[kind] = func(target, turn uuid.UUID) error {
			req := session.SessionControlRequest{EnvironmentID: f.env, SessionID: target, Kind: kind, IdempotencyKey: "nondisclosure-control"}
			if kind == "turn_interrupt" {
				req.TurnID = turn
			}
			if kind == "resume" {
				req.HoldID = uuid.NewV7()
			}
			_, err := session.ControlSession(t.Context(), policy, f.pool, caller, req)
			return err
		}
	}
	for name, invoke := range operations {
		t.Run(name, func(t *testing.T) {
			for _, target := range []uuid.UUID{invisible.session, uuid.NewV7()} {
				err := invoke(target, hiddenTurn.TurnID)
				if !errors.Is(err, session.ErrDenied) || errors.Is(err, session.ErrRuntimeMCPDenied) {
					t.Fatalf("invisible or absent target changed denial classification: %v", err)
				}
			}
		})
	}
	// Exact-Turn observation has an additional visibility boundary even when the
	// caller can otherwise reach the Session. Scope rejection follows that check.
	for _, turn := range []uuid.UUID{callerHumanTurn.TurnID, uuid.NewV7()} {
		_, err := session.RuntimeObserveTurn(t.Context(), policy, f.pool, caller, f.session, turn)
		if !errors.Is(err, session.ErrDenied) || errors.Is(err, session.ErrRuntimeMCPDenied) {
			t.Fatalf("unattributed or absent Turn changed denial classification: %v", err)
		}
	}
	// Making this target owned exposes its identity through existing base checks;
	// finite policy rejection must still be reported for every operation family.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$3,requester_session_id=$3,root_session_id=$3,causal_depth=1 WHERE environment_id=$1 AND id=$2`, f.env, invisible.session, f.session)
	for name, invoke := range operations {
		t.Run(name+"/visible_policy_denial", func(t *testing.T) {
			if err := invoke(invisible.session, hiddenTurn.TurnID); !errors.Is(err, session.ErrRuntimeMCPDenied) {
				t.Fatalf("visible target lost policy rejection: %v", err)
			}
		})
	}
	// Requested work is inspectable/enqueueable, but exact-Turn steering and
	// controls still require ownership before reporting any authored-policy error.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=NULL,root_session_id=id WHERE environment_id=$1 AND id=$2`, f.env, invisible.session)
	for _, name := range []string{"send_turn", "interrupt", "resume", "close", "cancel", "turn_interrupt"} {
		if err := operations[name](invisible.session, hiddenTurn.TurnID); !errors.Is(err, session.ErrDenied) || errors.Is(err, session.ErrRuntimeMCPDenied) {
			t.Fatalf("%s exposed a requested-only target through policy error: %v", name, err)
		}
	}
}
