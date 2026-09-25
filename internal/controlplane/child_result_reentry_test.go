package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"uuid"
)

type childReentryStore struct {
	db.Querier
	child         db.Run
	waits         map[int32]db.RunWait
	registrations []db.RegisterResolvedChildCallParams
}

func (s *childReentryStore) GetBoundSameWorkspaceChildCallReplay(context.Context, db.GetBoundSameWorkspaceChildCallReplayParams) (db.RunWait, error) {
	return db.RunWait{}, pgx.ErrNoRows
}
func (s *childReentryStore) GetChildCallAttemptWait(_ context.Context, p db.GetChildCallAttemptWaitParams) (db.RunWait, error) {
	w, ok := s.waits[p.AttemptNumber]
	if !ok {
		return w, pgx.ErrNoRows
	}
	return w, nil
}
func (s *childReentryStore) GetChildCallRunWaitReplay(_ context.Context, p db.GetChildCallRunWaitReplayParams) (db.RunWait, error) {
	w, ok := s.waits[p.AttemptNumber]
	if !ok || w.ID != p.ID {
		return db.RunWait{}, pgx.ErrNoRows
	}
	return w, nil
}
func (s *childReentryStore) GetRun(context.Context, db.GetRunParams) (db.Run, error) {
	return s.child, nil
}
func (s *childReentryStore) RegisterResolvedChildCall(_ context.Context, p db.RegisterResolvedChildCallParams) (db.RunWait, error) {
	s.registrations = append(s.registrations, p)
	w := db.RunWait{ID: p.ID, RunID: p.RunID, AttemptNumber: p.AttemptNumber, ChildRunID: p.ChildRunID, ResumeAttachID: p.ResumeAttachID, ConditionStatus: db.WaitStatusCompleted, ConditionResult: p.ConditionResult, SuspensionStatus: db.RunWaitStatusReleased}
	s.waits[p.AttemptNumber] = w
	return w, nil
}
func (s *childReentryStore) GetRunWait(_ context.Context, p db.GetRunWaitParams) (db.RunWait, error) {
	return s.waits[p.AttemptNumber], nil
}

func TestChildResultReentryCreatesOnlyAttemptLocalWait(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate Computer", true: "shared Computer"}[same], func(t *testing.T) {
			parent, child, ws, claim := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
			childWS := uuid.NewV7()
			if same {
				childWS = ws
			}
			a := runLeaseClaimAuthority{run: db.Run{ID: pgvalue.UUID(parent), EnvironmentID: pgvalue.UUID(uuid.NewV7()), WorkspaceID: pgvalue.UUID(ws), EntrypointKind: "task"}}
			store := &childReentryStore{child: db.Run{ID: pgvalue.UUID(child), WorkspaceID: pgvalue.UUID(childWS), ParentRunID: pgvalue.UUID(parent), ParentOwnsLifecycle: pgtype.Bool{Bool: true, Valid: true}, ClaimID: pgvalue.UUID(claim), Status: db.RunStatusSucceeded, Output: json.RawMessage(`{"reportPath":"/workspace/report.pdf"}`)}, waits: map[int32]db.RunWait{}}
			invoke := func(input childTaskInvokeInput) (workerapi.CreateRunWaitResponse, error) {
				if same {
					return replayBoundSameWorkspaceChildCall(t.Context(), store, input, a, db.IdempotencyClaim{ID: pgvalue.UUID(claim)}, "", idempotency.TaskChildInvokeFingerprint{}, childTaskReceipt{RunID: child.String(), WorkspaceID: childWS.String()})
				}
				return registerChildCall(t.Context(), store, input, a, db.IdempotencyClaim{ID: pgvalue.UUID(claim)}, idempotency.TaskChildInvokeFingerprint{}, taskStartResult{RunID: child, Replayed: true}, childWS)
			}
			for _, attempt := range []int32{1, 2} {
				a.attempt.Number = attempt
				input := childTaskInvokeInput{RunWaitID: uuid.NewV7(), ResumeAttachID: uuid.NewV7()}
				for range 2 {
					got, err := invoke(input)
					if err != nil || got.RunWaitID != input.RunWaitID.String() || got.ResolutionKind != "completed" {
						t.Fatalf("reentry=%+v %v", got, err)
					}
				}
				conflict := input
				conflict.RunWaitID = uuid.NewV7()
				if _, err := invoke(conflict); !errors.Is(err, errTaskStartReceiptInvalid) {
					t.Fatalf("same-attempt waiter replaced: %v", err)
				}
			}
			if len(store.registrations) != 2 || store.registrations[0].ChildRunID != store.registrations[1].ChildRunID || store.registrations[0].ID == store.registrations[1].ID {
				t.Fatal("logical child or attempt wait identity changed incorrectly")
			}
		})
	}
}
