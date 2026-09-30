package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrTaskCompletionAdmission = errors.New("Task completion admission is unavailable")

// TaskCompletion records a member outcome, never a Computer disk publication.
// Fingerprint covers the entire normalized request, including the operation ID.
type TaskCompletion struct {
	Fence       ExecutionFence
	OperationID pgtype.UUID
	Fingerprint string
	Kind        string
	Output      json.RawMessage
	Error       json.RawMessage
}

// CompleteTaskExecution atomically settles the attempt and its dependent waits,
// or schedules its pinned retry policy. The caller owns commit and rollback.
// Physical cleanup remains separately evidenced before retry admission.
func CompleteTaskExecution(ctx context.Context, tx pgx.Tx, request TaskCompletion) error {
	if !request.OperationID.Valid || request.Fingerprint == "" {
		return pgx.ErrNoRows
	}
	status, leaseStatus, outcome, reason := db.RunStatusFailed, db.RunLeaseStatusFailed, "failed", "task_failed"
	var failure json.RawMessage
	switch request.Kind {
	case "succeeded":
		if !json.Valid(request.Output) || len(request.Error) != 0 {
			return errors.New("invalid Task success")
		}
		status, leaseStatus, outcome, reason = db.RunStatusSucceeded, db.RunLeaseStatusCompleted, "succeeded", "completed"
	case "failed", "payload_invalid":
		if len(request.Output) != 0 {
			return errors.New("invalid Task failure output")
		}
		if request.Kind == "payload_invalid" {
			reason = "task_payload_invalid"
		}
		var body struct {
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		}
		if err := json.Unmarshal(request.Error, &body); err != nil || body.Message == "" {
			return errors.New("invalid Task failure")
		}
		var err error
		if len(body.Details) == 0 {
			body.Details = json.RawMessage(`{}`)
		}
		var details map[string]json.RawMessage
		if e := json.Unmarshal(body.Details, &details); e != nil || details == nil {
			return errors.New("invalid Task failure details")
		}
		failure, err = json.Marshal(struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		}{reason, body.Message, body.Details})
		if err != nil {
			return err
		}
	default:
		return errors.New("invalid Task outcome")
	}
	q := db.New(tx)
	// A durable receipt survives lease expiry and Computer reclamation. It grants
	// no authority to do more work and cannot accept a different terminal payload.
	replay, err := q.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{RunLeaseID: request.Fence.LeaseID, LeaseSequence: request.Fence.LeaseSequence, WorkerGroupID: request.Fence.WorkerGroupID, WorkerHostID: request.Fence.WorkerHostID})
	if err == nil {
		if replay.String != request.Fingerprint {
			return pgx.ErrNoRows
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: request.Fence.LeaseID, LeaseSequence: request.Fence.LeaseSequence, WorkerGroupID: request.Fence.WorkerGroupID, WorkerHostID: request.Fence.WorkerHostID, WorkerEpoch: request.Fence.WorkerEpoch})
	if err != nil {
		return fmt.Errorf("Task completion locate: %w", err)
	}
	secrets, err := secret.LockAttemptDelivery(ctx, q, loc.RunID, loc.AttemptNumber, loc.ComputerID)
	if err != nil {
		if errors.Is(err, secret.ErrDeliveryUnavailable) {
			return errors.Join(ErrTaskCompletionAdmission, err)
		}
		return fmt.Errorf("Task completion Secrets: %w", err)
	}
	graph, err := LockOwnedFinalizationWithInstanceFence(ctx, tx, OwnedFinalizationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID), RunID: pgvalue.MustUUIDValue(loc.RunID)}, func() error { return workergroup.LockExecutionHost(ctx, q, request.Fence.host(loc.RegionID, false)) })
	if err != nil {
		return fmt.Errorf("Task completion graph: %w", err)
	}
	// A competing identical request may have committed while graph locks waited.
	replay, err = q.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{RunLeaseID: request.Fence.LeaseID, LeaseSequence: request.Fence.LeaseSequence, WorkerGroupID: request.Fence.WorkerGroupID, WorkerHostID: request.Fence.WorkerHostID})
	if err == nil {
		if replay.String != request.Fingerprint {
			return pgx.ErrNoRows
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	a, err := lockExecution(ctx, tx, request.Fence, executionLive, executionTarget{})
	if err != nil {
		return fmt.Errorf("Task completion authority: %w", err)
	}
	r, l := a.Run, a.Lease
	if r.EntrypointKind != "task" || r.SessionID.Valid || r.Status != db.RunStatusRunning || l.Status != db.RunLeaseStatusFinalizing || l.FinalizationOperationID != request.OperationID || !l.FinalizationStartedAt.Valid || !a.Attempt.EntrypointEnteredAt.Valid || r.ActiveStartedAt.Valid {
		return pgx.ErrNoRows
	}
	clear, err := q.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{RunID: r.ID, AttemptNumber: a.Attempt.Number, ComputerID: r.ComputerID})
	if err != nil {
		return err
	}
	if !clear {
		return pgx.ErrNoRows
	}
	now, err := q.GetTaskCompletionTime(ctx)
	if err != nil {
		return err
	}
	if !now.Valid || !now.Time.Before(l.ExpiresAt.Time) || !now.Time.Before(a.Instance.WriterExpiresAt.Time) || l.FinalizationStartedAt.Time.After(now.Time) {
		return pgx.ErrNoRows
	}
	delay, again := time.Duration(0), false
	if request.Kind == "failed" && r.ActiveElapsedMs < r.MaxActiveDurationMs {
		policy, e := definition.ParseRetry(r.RetryPolicy)
		if e != nil {
			return errors.Join(ErrTaskCompletionAdmission, e)
		}
		delay, again, err = definition.RetryDelay(policy, a.Attempt.Number, nil)
		if err != nil {
			return err
		}
	}
	if !again {
		if _, err = graph.CancelDescendants(ctx); err != nil {
			return err
		}
	}
	if _, err = q.CompleteTaskRunLease(ctx, db.CompleteTaskRunLeaseParams{Status: leaseStatus, CompletedAt: now, ReasonCode: pgvalue.Text(reason), Error: request.Error, TerminalRequestFingerprint: pgvalue.Text(request.Fingerprint), ID: l.ID, RunID: r.ID, ComputerID: r.ComputerID, AttemptNumber: a.Attempt.Number, LeaseSequence: l.LeaseSequence}); err != nil {
		return fmt.Errorf("Task completion lease: %w", err)
	}
	if _, err = q.CompleteTaskAttempt(ctx, db.CompleteTaskAttemptParams{TerminalOutcome: pgvalue.Text(outcome), ReasonCode: pgvalue.Text(reason), Error: request.Error, CompletedAt: now, RunID: r.ID, Number: a.Attempt.Number, ComputerID: r.ComputerID}); err != nil {
		return fmt.Errorf("Task completion attempt: %w", err)
	}
	if again {
		// The saved head is recovery provenance, not the concurrently changing live
		// disk. Retry neither captures that disk nor replaces the physical writer.
		next := a.Attempt.Number + 1
		if _, err = q.CreateTaskRetryAttempt(ctx, db.CreateTaskRetryAttemptParams{ResultComputerDiskVersionID: a.Computer.HeadDiskVersionID, Number: next, RunID: r.ID, ComputerID: r.ComputerID, PreviousAttemptNumber: a.Attempt.Number, RunLeaseID: l.ID}); err != nil {
			return err
		}
		resolutions := make([]secret.Resolution, 0, len(secrets))
		for _, binding := range secrets {
			if !binding.Secret.CurrentVersionID.Valid || binding.Secret.Status != "active" {
				return errors.Join(ErrTaskCompletionAdmission, secret.ErrDeliveryUnavailable)
			}
			resolutions = append(resolutions, secret.Resolution{PlacementKind: binding.PlacementKind, PlacementTarget: binding.PlacementTarget, SecretID: binding.Secret.ID, SecretVersionID: binding.Secret.CurrentVersionID, RevocationGeneration: binding.Secret.RevocationGeneration})
		}
		if err = secret.CreateAttemptResolutions(ctx, q, r.ComputerID, r.ID, next, resolutions); err != nil {
			return err
		}
		if _, err = q.DelayTaskRunRetry(ctx, db.DelayTaskRunRetryParams{ResultComputerDiskVersionID: a.Computer.HeadDiskVersionID, NextAttemptNumber: next, CompletedAt: now, RetryAt: pgvalue.Timestamptz(now.Time.Add(delay)), ID: r.ID, ComputerID: r.ComputerID, PreviousAttemptNumber: a.Attempt.Number, RunLeaseID: l.ID}); err != nil {
			return err
		}
	} else {
		if _, err = q.FinishTaskRun(ctx, db.FinishTaskRunParams{Status: status, Output: request.Output, Failure: failure, CompletedAt: now, ID: r.ID, ComputerID: r.ComputerID, AttemptNumber: a.Attempt.Number, RunLeaseID: l.ID}); err != nil {
			return fmt.Errorf("Task completion Run: %w", err)
		}
		event, payload := "run.failed", []byte(`{"reason":"`+reason+`"}`)
		if status == db.RunStatusSucceeded {
			event, payload = "run.completed", []byte(`{}`)
		}
		if _, err = q.AppendRunEvent(ctx, db.AppendRunEventParams{OrgID: r.OrgID, RunID: r.ID, Kind: event, Payload: payload}); err != nil {
			return err
		}
		if wait, ok := graph.waitsByChild[pgvalue.MustUUIDValue(r.ID)]; ok && wait.conditionStatus == db.WaitStatusPending {
			parent, ok := graph.locked[pgvalue.MustUUIDValue(r.ParentRunID)]
			if !ok {
				return pgx.ErrNoRows
			}
			result := struct {
				OK      bool            `json:"ok"`
				Output  json.RawMessage `json:"output,omitempty"`
				Failure json.RawMessage `json:"failure,omitempty"`
				Run     struct {
					ID string `json:"id"`
				} `json:"run"`
			}{OK: status == db.RunStatusSucceeded, Output: request.Output, Failure: failure}
			result.Run.ID = pgvalue.UUIDString(r.ID)
			raw, e := json.Marshal(result)
			if e != nil {
				return e
			}
			if err = resolveChildResult(ctx, tx, parent, wait, raw); err != nil {
				return err
			}
		}
	}
	// Locks exclude authority changes, but cannot stop time advancing during writes.
	var live bool
	if err = tx.QueryRow(ctx, `SELECT l.expires_at>clock_timestamp() AND i.writer_expires_at>clock_timestamp() FROM run_leases l JOIN computer_instances i ON i.id=l.computer_instance_id WHERE l.id=$1`, l.ID).Scan(&live); err != nil {
		return err
	}
	if !live {
		return pgx.ErrNoRows
	}
	return nil
}
