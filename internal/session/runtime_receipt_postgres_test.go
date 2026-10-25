package session_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"

	"github.com/helmrdotdev/helmr/internal/session"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeReceiptRecoveryRequiresCurrentGeneration(t *testing.T) {
	for _, operation := range []string{"create", "spawn", "enqueue", "interrupt"} {
		t.Run(operation, func(t *testing.T) {
			f := newAdmissionFixture(t)
			caller := runtimeReadCaller(t, f)
			target := f.peer(t)
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET parent_session_id=$1,requester_session_id=$1,root_session_id=$1,causal_depth=1 WHERE id=$2`, f.session, target.session)
			var original string
			invoke := func() (string, error) {
				switch operation {
				case "create", "spawn":
					req := f.startRequest("original-key")
					req.ComputerID = f.computer
					receipt, err := session.AdmitSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, req, operation)
					return receipt.TurnID.String(), err
				case "enqueue":
					receipt, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, nil, caller, session.EnqueueRequest{EnvironmentID: f.env, SessionID: target.session, IdempotencyKey: "original-key", Input: []byte(`[]`)})
					return receipt.TurnID.String(), err
				default:
					receipt, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.pool, caller, session.SessionControlRequest{EnvironmentID: f.env, SessionID: target.session, Kind: "interrupt", IdempotencyKey: "original-key"})
					return receipt.ID.String(), err
				}
			}
			var err error
			if original, err = invoke(); err != nil {
				t.Fatal(err)
			}
			if err := closeProcessingForTest(t.Context(), f.pool, f.execution(), caller.TurnID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=2 WHERE id=$1`, f.session)
			if _, err := invoke(); !errors.Is(err, session.ErrDenied) {
				t.Fatalf("stale authority read receipt: %v", err)
			}
			caller.Execution.AuthorityGeneration = 2
			if recovered, err := invoke(); err != nil || recovered != original {
				t.Fatalf("renewed receipt %s expected %s: %v", recovered, original, err)
			}
		})
	}
}
