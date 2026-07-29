package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeWorkerChildTaskRequestUsesParentScopeAndCallerOptions(t *testing.T) {
	workspaceID := uuid.Must(uuid.NewV7()).String()
	concurrencyKey := "customer:1"
	normalized, err := normalizeWorkerChildTaskRequest(
		api.WorkerInvokeChildTaskRequest{
			TaskDeclaredID: "resize-image",
			PayloadPresent: true,
			Payload:        json.RawMessage(`{"b":2,"a":1}`),
			Workspace:      json.RawMessage(`{"id":"` + workspaceID + `"}`),
			Options: json.RawMessage(`{
				"queue":"images",
				"concurrency_key":"customer:1",
				"priority":10,
				"ttl":"1m",
				"retry":{
					"max_attempts":3,
					"backoff":{"min_delay":"1s","max_delay":"30s","factor":2,"jitter":"full"}
				},
				"metadata":{"source":"parent"},
				"tags":[" resize ","image","image"]
			}`),
			IdempotencyKey: "resize:image-1",
		},
		db.GetLiveRunLeaseLocatorsRow{
			OrgID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.Payload) != `{"a":1,"b":2}` ||
		normalized.QueueName != "images" ||
		normalized.ConcurrencyKey == nil ||
		*normalized.ConcurrencyKey != concurrencyKey ||
		normalized.Priority != 10 ||
		normalized.QueuedTTLMS == nil ||
		*normalized.QueuedTTLMS != 60_000 ||
		string(normalized.RetryPolicy) != `{"backoff":{"factor":2,"jitter":"full","maxMs":30000,"minMs":1000},"enabled":true,"maxAttempts":3}` ||
		normalized.IdempotencyKey != "resize:image-1" {
		t.Fatalf("normalized = %+v", normalized)
	}
	if len(normalized.Tags) != 2 ||
		normalized.Tags[0] != "image" ||
		normalized.Tags[1] != "resize" {
		t.Fatalf("tags = %#v", normalized.Tags)
	}
}

func TestSourceChildWorkspaceRequiresExactPair(t *testing.T) {
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sourceID := uuid.Must(uuid.NewV7())
	targetID := uuid.Must(uuid.NewV7())
	source := db.Workspace{ID: pgvalue.UUID(sourceID), EnvironmentID: environmentID}
	target := db.Workspace{ID: pgvalue.UUID(targetID), EnvironmentID: environmentID}
	locators := db.GetLiveRunLeaseLocatorsRow{EnvironmentID: environmentID}

	got, err := sourceChildWorkspace(
		[]db.Workspace{target, source},
		sourceID,
		targetID,
		locators,
	)
	if err != nil || got.ID != source.ID {
		t.Fatalf("sourceChildWorkspace() = %+v, %v", got, err)
	}
	if _, err := sourceChildWorkspace(
		[]db.Workspace{source},
		sourceID,
		targetID,
		locators,
	); !errors.Is(err, errTaskWorkspaceUnavailable) {
		t.Fatalf("missing target error = %v", err)
	}
}

func TestDecodeChildTaskReceiptRequiresCanonicalAuthority(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	receipt, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","workspaceId":"` + workspaceID.String() + `"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID != runID.String() ||
		receipt.WorkspaceID != workspaceID.String() {
		t.Fatalf("receipt = %+v", receipt)
	}
	if _, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","workspaceId":"00000000-0000-0000-0000-000000000000"}`,
	)); !errors.Is(err, errTaskStartReceiptInvalid) {
		t.Fatalf("nil Workspace receipt error = %v", err)
	}
}

func TestReplayBoundSameWorkspaceChildCallUsesReceiptAuthority(t *testing.T) {
	environmentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	parentRunID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	childRunID := uuid.Must(uuid.NewV7())
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitID := uuid.Must(uuid.NewV7())
	baseID := uuid.Must(uuid.NewV7())
	claimID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	resumeAttachID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	const digest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	store := &sameWorkspaceChildReplayStore{
		wait: db.RunWait{
			ID:                       pgvalue.UUID(waitID),
			EnvironmentID:            environmentID,
			RunID:                    parentRunID,
			WorkspaceID:              workspaceID,
			ChildRunID:               pgvalue.UUID(childRunID),
			SuspensionState:          db.RunWaitStateReleased,
			ConditionState:           db.WaitStateCompleted,
			ConditionResult:          json.RawMessage(`{"ok":true,"output":7}`),
			ResumeAttachID:           resumeAttachID,
			ResumeWorkspaceVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		},
	}
	response, err := replayBoundSameWorkspaceChildCall(
		t.Context(),
		store,
		childTaskInvokeInput{
			Request: api.WorkerInvokeChildTaskRequest{
				Lease: api.WorkerRunLeaseFence{
					ID: uuid.Must(uuid.NewV7()).String(), LeaseSequence: 1,
				},
			},
			Normalized: normalizedTaskStart{
				taskStartRequest: taskStartRequest{TaskDeclaredID: "child"},
			},
			RunWaitID:      waitID,
			ResumeAttachID: pgvalue.MustUUIDValue(resumeAttachID),
		},
		runLeaseClaimAuthority{run: db.Run{
			ID:            parentRunID,
			EnvironmentID: environmentID,
			WorkspaceID:   workspaceID,
		}},
		db.IdempotencyClaim{ID: claimID},
		"sha256:"+strings.Repeat("0", 64),
		childTaskReceipt{
			RunID:                  childRunID.String(),
			RunWaitID:              waitID.String(),
			ResumeAttachID:         pgvalue.MustUUIDValue(resumeAttachID).String(),
			BaseWorkspaceVersionID: baseID.String(),
			BaseWorkspaceDigest:    digest,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.RunWaitID != waitID.String() ||
		response.ResumeAttachID != pgvalue.MustUUIDValue(resumeAttachID).String() ||
		response.ResolutionKind != "completed" ||
		string(response.Resolution) != `{"ok":true,"output":7}` {
		t.Fatalf("response = %+v", response)
	}
	if store.params.ID != pgvalue.UUID(waitID) ||
		store.params.ChildRunID != pgvalue.UUID(childRunID) ||
		store.params.BaseWorkspaceVersionID != pgvalue.UUID(baseID) ||
		store.params.BaseWorkspaceContentDigest.String != digest {
		t.Fatalf("receipt authority query = %+v", store.params)
	}
}

func TestReplayBoundSameWorkspaceChildCallRejectsDifferentFrontier(t *testing.T) {
	_, err := replayBoundSameWorkspaceChildCall(
		t.Context(),
		&sameWorkspaceChildReplayStore{err: pgx.ErrNoRows},
		childTaskInvokeInput{
			Request: api.WorkerInvokeChildTaskRequest{
				Lease: api.WorkerRunLeaseFence{
					ID: uuid.Must(uuid.NewV7()).String(), LeaseSequence: 1,
				},
			},
			Normalized: normalizedTaskStart{
				taskStartRequest: taskStartRequest{TaskDeclaredID: "child"},
			},
			RunWaitID:      uuid.Must(uuid.NewV7()),
			ResumeAttachID: uuid.Must(uuid.NewV7()),
		},
		runLeaseClaimAuthority{run: db.Run{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			WorkspaceID:   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		}},
		db.IdempotencyClaim{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))},
		"sha256:"+strings.Repeat("0", 64),
		childTaskReceipt{
			RunID:                  uuid.Must(uuid.NewV7()).String(),
			RunWaitID:              uuid.Must(uuid.NewV7()).String(),
			ResumeAttachID:         uuid.Must(uuid.NewV7()).String(),
			BaseWorkspaceVersionID: uuid.Must(uuid.NewV7()).String(),
			BaseWorkspaceDigest:    "sha256:" + strings.Repeat("0", 64),
		},
	)
	if !errors.Is(err, errWorkspaceHandoffConflict) {
		t.Fatalf("different frontier error = %v", err)
	}
}

type sameWorkspaceChildReplayStore struct {
	db.Querier
	params db.GetBoundSameWorkspaceChildCallReplayParams
	wait   db.RunWait
	err    error
}

func (s *sameWorkspaceChildReplayStore) GetBoundSameWorkspaceChildCallReplay(
	_ context.Context,
	params db.GetBoundSameWorkspaceChildCallReplayParams,
) (db.RunWait, error) {
	s.params = params
	return s.wait, s.err
}

func TestChildTaskResultProjectsTerminalOutcome(t *testing.T) {
	runID := uuid.Must(uuid.NewV7()).String()

	tests := []struct {
		name string
		run  db.Run
		want string
	}{
		{
			name: "success",
			run: db.Run{
				ID:     pgvalue.UUID(uuid.MustParse(runID)),
				Status: db.RunStatusSucceeded,
				Output: json.RawMessage(`{"value":7}`),
			},
			want: `{"ok":true,"output":{"value":7},"run":{"id":"` + runID + `"}}`,
		},
		{
			name: "failure",
			run: db.Run{
				ID:                 pgvalue.UUID(uuid.MustParse(runID)),
				Status:             db.RunStatusFailed,
				TerminalReasonCode: pgtype.Text{String: "dependency_failed", Valid: true},
				Error: json.RawMessage(
					`{"code":"upstream_failed","message":"upstream failed","retryable":true,"details":{"service":"images"}}`,
				),
			},
			want: `{"ok":false,"error":{"code":"upstream_failed","message":"upstream failed","retryable":true,"details":{"service":"images"}},"run":{"id":"` + runID + `"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := childTaskResult(test.run)
			if err != nil {
				t.Fatalf("childTaskResult() error = %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("childTaskResult() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestChildTaskResultRejectsIncompleteTerminalState(t *testing.T) {
	_, err := childTaskResult(db.Run{
		ID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Status: db.RunStatusFailed,
	})
	if err == nil {
		t.Fatal("childTaskResult() accepted a failed Run without a terminal reason")
	}
}

func TestParentOwnedChildMayFinishBetweenParentAttempts(t *testing.T) {
	store := &parentOwnedChildWaitStore{err: pgx.ErrNoRows}
	params := db.LockParentOwnedChildWaitParams{}
	for _, status := range []db.RunStatus{
		db.RunStatusQueued,
		db.RunStatusRunning,
		db.RunStatusWaiting,
		db.RunStatusRetryDelayed,
	} {
		wait, err := lockParentOwnedChildWaitIfActive(
			t.Context(), store, db.Run{Status: status}, params,
		)
		if err != nil || wait.ID.Valid {
			t.Fatalf("parent status %s = wait:%+v error:%v", status, wait, err)
		}
	}
	if _, err := lockParentOwnedChildWaitIfActive(
		t.Context(), store, db.Run{Status: db.RunStatusCancelRequested}, params,
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("cancelling parent without active child Wait error = %v", err)
	}
}

type parentOwnedChildWaitStore struct {
	db.Querier
	wait db.RunWait
	err  error
}

func (s *parentOwnedChildWaitStore) LockParentOwnedChildWait(
	context.Context,
	db.LockParentOwnedChildWaitParams,
) (db.RunWait, error) {
	return s.wait, s.err
}
