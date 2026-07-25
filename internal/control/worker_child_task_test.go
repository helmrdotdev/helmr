package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNormalizeWorkerChildTaskRequestUsesParentScopeAndCallerOptions(t *testing.T) {
	workspaceID := actorStartTestPublicID(t, publicid.Workspace)
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

func TestChildWorkspacePairLockIsOrderIndependent(t *testing.T) {
	left := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	right := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	if childWorkspacePairLock(left, right) != childWorkspacePairLock(right, left) {
		t.Fatal("Workspace pair lock depends on argument order")
	}
	if childWorkspacePairLock(left, right) == childWorkspacePairLock(left, left) {
		t.Fatal("distinct Workspace pairs produced the same lock key")
	}
}

func TestDecodeChildTaskReceiptRequiresCanonicalAuthority(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	runPublicID := actorStartTestPublicID(t, publicid.Run)
	receipt, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","runPublicId":"` + runPublicID +
			`","workspaceId":"` + workspaceID.String() + `"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID != runID.String() ||
		receipt.RunPublicID != runPublicID ||
		receipt.WorkspaceID != workspaceID.String() {
		t.Fatalf("receipt = %+v", receipt)
	}
	if _, err := decodeChildTaskReceipt([]byte(
		`{"runId":"` + runID.String() +
			`","runPublicId":"` + runPublicID +
			`","workspaceId":"00000000-0000-0000-0000-000000000000"}`,
	)); !errors.Is(err, errTaskStartReceiptInvalid) {
		t.Fatalf("nil Workspace receipt error = %v", err)
	}
}

func TestChildTaskResultProjectsTerminalOutcome(t *testing.T) {
	const runID = "run_aaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name string
		run  db.Run
		want string
	}{
		{
			name: "success",
			run: db.Run{
				PublicID: runID,
				Status:   db.RunStatusSucceeded,
				Output:   json.RawMessage(`{"value":7}`),
			},
			want: `{"ok":true,"output":{"value":7},"run":{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
		{
			name: "failure",
			run: db.Run{
				PublicID:           runID,
				Status:             db.RunStatusFailed,
				TerminalReasonCode: pgtype.Text{String: "dependency_failed", Valid: true},
				Error: json.RawMessage(
					`{"code":"upstream_failed","message":"upstream failed","retryable":true,"details":{"service":"images"}}`,
				),
			},
			want: `{"ok":false,"error":{"code":"upstream_failed","message":"upstream failed","retryable":true,"details":{"service":"images"}},"run":{"id":"run_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
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
		PublicID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:   db.RunStatusFailed,
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
