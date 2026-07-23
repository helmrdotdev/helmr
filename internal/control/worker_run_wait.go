package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	rootTokenWaitHotWindow      = 2 * time.Minute
	defaultTokenWaitIdleTimeout = 30 * time.Second
	maxTokenWaitIdleTimeout     = time.Hour
	maxTokenWaitTimeout         = 365 * 24 * time.Hour
	shortWaitGrace              = time.Second
)

type workerTokenWaitParams struct {
	TokenID string `json:"token_id"`
}

func (s *Server) workerCreateRunWait(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCreateRunWaitRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Run Wait JSON: %w", err)))
		return
	}
	if request.Kind != api.WorkerRunWaitKindToken {
		writeError(w, badRequest(fmt.Errorf("Run Wait kind %q is not implemented by the durable runtime", request.Kind)))
		return
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var params workerTokenWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token Wait params: %w", err)))
		return
	}
	tokenID, err := parseCanonicalUUID("params.token_id", params.TokenID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, tags, err := normalizeRunWaitPresentation(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	timeoutAt, idleTimeout, checkpointDueAt, checkpointDelay, err := runWaitDeadlines(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.Params, err = json.Marshal(workerTokenWaitParams{TokenID: tokenID.String()})
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize Token Wait params: %w", err)))
		return
	}
	normalized.Metadata = metadata
	normalized.Tags = tags
	fingerprint, err := terminalRequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint Token Wait registration: %w", err)))
		return
	}
	parsed, worker, locators, run, err := s.loadRunWaitRegistrationAuthority(r.Context(), normalized.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	waitID := derivedRunWaitID(parsed.runID, request.Lease.AttemptNumber, correlationID, "wait")
	resumeAttachID := derivedRunWaitID(parsed.runID, request.Lease.AttemptNumber, correlationID, "resume-attach")
	reconcileDB, ok := s.tx.(db.TokenWaitReconcileDB)
	if !ok {
		writeError(w, unavailable(errors.New("durable Token Wait storage is not configured")))
		return
	}
	reconciler, err := db.NewTokenWaitReconciler(reconcileDB)
	if err != nil {
		writeError(w, unavailable(err))
		return
	}
	actorCursor := pgtype.Int8{}
	if request.ActorSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{Int64: *request.ActorSpeculativeInputSequence, Valid: true}
	}
	registered, err := reconciler.RegisterWait(r.Context(), db.TokenWaitRegistration{
		EnvironmentID: uuid.UUID(locators.EnvironmentID.Bytes), RunID: parsed.runID,
		TokenID: tokenID, WaitID: waitID, ResumeAttachID: resumeAttachID,
		ExpectedRunStateVersion: run.StateVersion, AttemptNumber: request.Lease.AttemptNumber,
		CurrentRunLeaseID: parsed.leaseID, LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: request.Lease.WorkerGroupID, WorkerInstanceID: parsed.workerInstanceID,
		WorkerEpoch: request.Lease.WorkerEpoch, WorkerProtocolVersion: request.Lease.WorkerProtocolVersion,
		RuntimeInstanceID: parsed.runtimeInstanceID, RuntimeIdentityID: request.Lease.RuntimeIdentityID,
		RegionID: locators.RegionID, NetworkSlotID: parsed.networkSlotID,
		NetworkSlotGeneration: request.Lease.NetworkSlotGeneration,
		WorkspaceMountID:      parsed.workspaceMountID, WorkspaceLeaseID: parsed.workspaceLeaseID,
		OwnershipGeneration: request.Lease.OwnershipGeneration, WriterGeneration: request.Lease.WriterGeneration,
		MountFencingGeneration:        request.Lease.MountFencingGeneration,
		RequestFingerprint:            fingerprint,
		ActorSpeculativeInputSequence: actorCursor,
		TimeoutAt:                     timeoutAt, IdleTimeoutMS: idleTimeout, CheckpointDueAt: checkpointDueAt,
		Metadata: metadata, Tags: tags,
	})
	if errors.Is(err, db.ErrTokenWaitReconcileAuthority) {
		writeError(w, conflict(errors.New("worker Run Wait receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("register worker Token Wait failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("register worker Token Wait"))
		return
	}
	response := api.WorkerCreateRunWaitResponse{
		RunID: request.Lease.RunID, RunWaitID: registered.WaitID.String(),
		ResumeAttachID: resumeAttachID.String(), RuntimeInstanceID: request.Lease.RuntimeInstanceID,
		RuntimeEpoch: request.Lease.WorkerEpoch, CheckpointDelayMs: checkpointDelay.Milliseconds(),
	}
	if registered.SuspensionState == db.RunWaitStateReleased {
		response.ResolutionKind, response.Resolution, err = tokenWaitDecision(
			registered.ConditionState, registered.Result, registered.ReasonCode,
		)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	_ = worker
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerPollRunWait(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRunWaitPollRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Run Wait poll JSON: %w", err)))
		return
	}
	parsed, worker, locators, err := s.loadRunWaitLeaseAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	wait, err := s.db.GetRunWait(r.Context(), db.GetRunWaitParams{
		RunID: locators.RunID, AttemptNumber: request.Lease.AttemptNumber, ID: pgvalue.UUID(waitID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker Run Wait is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker Run Wait"))
		return
	}
	if wait.Kind != db.WaitKindToken || wait.AttemptNumber != request.Lease.AttemptNumber ||
		wait.WorkspaceID != locators.WorkspaceID ||
		(wait.CurrentRunLeaseID != pgvalue.UUID(parsed.leaseID) && wait.PriorRunLeaseID != pgvalue.UUID(parsed.leaseID)) {
		writeError(w, conflict(errors.New("worker Run Wait fence is stale")))
		return
	}
	response := api.WorkerRunWaitPollResponse{RunID: request.Lease.RunID, RunWaitID: waitID.String()}
	switch wait.SuspensionState {
	case db.RunWaitStateReleased:
		response.Status = api.WorkerRunWaitPollStatusResumeRequested
		response.ResumeKind, response.ResumePayload, err = tokenWaitDecision(
			wait.ConditionState, wait.ConditionResult, pgvalue.TextValue(wait.ConditionReasonCode),
		)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
		response.RequireAck = false
	case db.RunWaitStateHot:
		if wait.ConditionState != db.WaitStatePending {
			writeError(w, conflict(errors.New("terminal hot Run Wait was not released")))
			return
		}
		if wait.CheckpointDueAt.Valid && !time.Now().Before(wait.CheckpointDueAt.Time) {
			wait, err = s.requestWorkerRunWaitCheckpoint(r.Context(), worker, request.Lease, parsed, waitID)
			if err != nil {
				if errors.Is(err, errStaleRunLeaseClaim) || isNoRows(err) {
					writeError(w, conflict(errors.New("worker Run Wait checkpoint request is stale")))
				} else {
					writeError(w, errors.New("request worker Run Wait checkpoint"))
				}
				return
			}
		}
		if wait.SuspensionState == db.RunWaitStateHot {
			response.Status = api.WorkerRunWaitPollStatusWaiting
			break
		}
		if !wait.SuspendCheckpointID.Valid || wait.CheckpointRequestVersion <= 0 {
			writeError(w, errors.New("checkpointing Run Wait has incomplete authority"))
			return
		}
		response.Status = api.WorkerRunWaitPollStatusCheckpointRequested
		response.RequestVersion = wait.CheckpointRequestVersion
		response.CheckpointID = pgvalue.UUIDString(wait.SuspendCheckpointID)
		response.CaptureWorkspace = true
	case db.RunWaitStateCheckpointing:
		if !wait.SuspendCheckpointID.Valid || wait.CheckpointRequestVersion <= 0 {
			writeError(w, errors.New("checkpointing Run Wait has incomplete authority"))
			return
		}
		response.Status = api.WorkerRunWaitPollStatusCheckpointRequested
		response.RequestVersion = wait.CheckpointRequestVersion
		response.CheckpointID = pgvalue.UUIDString(wait.SuspendCheckpointID)
		response.CaptureWorkspace = true
	default:
		response.Status = api.WorkerRunWaitPollStatusTerminal
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerAcknowledgeRunWaitResume(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRunWaitResumeAckRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Run Wait resume acknowledgement JSON: %w", err)))
		return
	}
	writeError(w, conflict(errors.New("hot Token Wait decisions do not require acknowledgement")))
}

func (s *Server) requestWorkerRunWaitCheckpoint(
	ctx context.Context,
	worker workerActor,
	receipt api.WorkerRunLeaseReceipt,
	parsed parsedRunLeaseReceipt,
	waitID uuid.UUID,
) (db.RunWait, error) {
	var updated db.RunWait
	err := s.inTx(ctx, func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(ctx, work.q, pgvalue.UUID(parsed.runID), receipt.AttemptNumber, pgvalue.UUID(parsed.workspaceID)); err != nil {
			return fmt.Errorf("lock Run Wait Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			ctx, work.q, worker, pgvalue.UUID(parsed.leaseID), receipt.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.run.Status != db.RunStatusWaiting || authority.run.ParentRunID.Valid ||
			authority.runLease.State != db.RunLeaseStateRunning {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
		})
		if err != nil || !equalCurrentOrPreviousRunLeaseReceipt(current, receipt, authority.runLease.PreviousExpiresAt) {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if err := validateRootRunWaitActorCursor(authority, wait); err != nil {
			return err
		}
		if wait.SuspensionState == db.RunWaitStateCheckpointing && wait.SuspendCheckpointID.Valid {
			updated = wait
			return nil
		}
		if wait.Kind != db.WaitKindToken || wait.ConditionState != db.WaitStatePending ||
			wait.SuspensionState != db.RunWaitStateHot || !wait.CheckpointDueAt.Valid {
			return errStaleRunLeaseClaim
		}
		checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		if _, err := work.q.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
			ID: checkpointID, Kind: db.RunCheckpointKindSuspend, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, RunWaitID: wait.ID,
			SourceRunLeaseID: authority.runLease.ID, SourceWorkspaceLeaseID: authority.workspaceLease.ID,
			WorkspaceID: authority.workspace.ID, BaseWorkspaceVersionID: authority.workspaceLease.BaseVersionID,
			ActorSpeculativeInputSequence: wait.ActorSpeculativeInputSequence,
			RestoreManifest:               []byte(`{}`),
		}); err != nil {
			return fmt.Errorf("create Run checkpoint intent: %w", err)
		}
		if _, err := work.q.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{
			ID: authority.runLease.ID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
			AttemptNumber: authority.attempt.Number, LeaseSequence: authority.runLease.LeaseSequence,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		updated, err = work.q.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{
			SuspendCheckpointID: checkpointID, RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			ID: wait.ID, CurrentRunLeaseID: authority.runLease.ID,
		})
		return err
	})
	return updated, err
}

func (s *Server) loadRunWaitRegistrationAuthority(
	ctx context.Context,
	receipt api.WorkerRunLeaseReceipt,
) (parsedRunLeaseReceipt, workerActor, db.GetLiveRunLeaseLocatorsRow, db.Run, error) {
	parsed, worker, locators, err := s.loadRunWaitLeaseAuthority(ctx, receipt)
	if err != nil {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, err
	}
	run, err := s.db.GetRun(ctx, db.GetRunParams{EnvironmentID: locators.EnvironmentID, ID: locators.RunID})
	if err != nil || (run.Status != db.RunStatusRunning && run.Status != db.RunStatusWaiting) ||
		(run.EntrypointKind != "task" && run.EntrypointKind != "actor") || run.ParentRunID.Valid ||
		(run.EntrypointKind == "task") != !run.ActorID.Valid || run.CurrentRunLeaseID != pgvalue.UUID(parsed.leaseID) {
		if err == nil {
			err = errors.New("run is not an active root Task or Actor")
		}
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, conflict(err)
	}
	return parsed, worker, locators, run, nil
}

func validateRootRunWaitActorCursor(authority runLeaseClaimAuthority, wait db.RunWait) error {
	switch authority.run.EntrypointKind {
	case "task":
		if authority.run.ActorID.Valid || wait.ActorSpeculativeInputSequence.Valid {
			return errStaleRunLeaseClaim
		}
	case "actor":
		cursor := wait.ActorSpeculativeInputSequence
		if !authority.run.ActorID.Valid || authority.run.ActorID != authority.actor.ID ||
			!authority.actor.CurrentRunID.Valid || authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") ||
			!authority.attempt.ActorStartInputSequence.Valid || !cursor.Valid ||
			authority.attempt.ActorStartInputSequence.Int64 > authority.actor.CommittedInputSequence ||
			cursor.Int64 < authority.actor.CommittedInputSequence ||
			cursor.Int64 > authority.actor.CommittedInputSequence+1 ||
			cursor.Int64 >= authority.actor.NextInputSequence ||
			authority.workspace.OwnerActorID != authority.actor.ID || authority.workspace.OwnerRunID.Valid {
			return errStaleRunLeaseClaim
		}
	default:
		return errStaleRunLeaseClaim
	}
	return nil
}

func (s *Server) loadRunWaitLeaseAuthority(
	ctx context.Context,
	receipt api.WorkerRunLeaseReceipt,
) (parsedRunLeaseReceipt, workerActor, db.GetLiveRunLeaseLocatorsRow, error) {
	parsed, err := parseRunLeaseReceipt(receipt)
	if err != nil {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, badRequest(err)
	}
	worker := workerFromContext(ctx)
	if receipt.WorkerGroupID != worker.WorkerGroupID || parsed.workerInstanceID != worker.WorkerInstanceID ||
		receipt.WorkerEpoch != worker.WorkerEpoch || receipt.WorkerProtocolVersion != worker.ProtocolVersion {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, forbidden(errors.New("worker Run Wait receipt belongs to another worker epoch"))
	}
	locators, err := s.db.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if isNoRows(err) {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, conflict(errors.New("worker Run Wait receipt is stale"))
	}
	if err != nil {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, errors.New("load worker Run Wait authority")
	}
	if locators.RunID != pgvalue.UUID(parsed.runID) || locators.WorkspaceID != pgvalue.UUID(parsed.workspaceID) ||
		locators.AttemptNumber != receipt.AttemptNumber || locators.RuntimeInstanceID != pgvalue.UUID(parsed.runtimeInstanceID) ||
		locators.NetworkSlotID != pgvalue.UUID(parsed.networkSlotID) || locators.NetworkSlotGeneration != receipt.NetworkSlotGeneration ||
		locators.WorkspaceMountID != pgvalue.UUID(parsed.workspaceMountID) || locators.WorkspaceLeaseID != pgvalue.UUID(parsed.workspaceLeaseID) {
		return parsedRunLeaseReceipt{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, conflict(errors.New("worker Run Wait receipt fence is stale"))
	}
	return parsed, worker, locators, nil
}

func derivedRunWaitID(runID uuid.UUID, attemptNumber int32, correlationID uuid.UUID, role string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf(
		"helmr.run-wait.v1:%s:%s:%d:%s", role, runID, attemptNumber, correlationID,
	)))
}

func runWaitDeadlines(request api.WorkerCreateRunWaitRequest) (pgtype.Timestamptz, pgtype.Int8, pgtype.Timestamptz, time.Duration, error) {
	now := time.Now().UTC()
	checkpointDelay := rootTokenWaitHotWindow
	var timeoutAt pgtype.Timestamptz
	if request.TimeoutSeconds != nil {
		if *request.TimeoutSeconds <= 0 || time.Duration(*request.TimeoutSeconds)*time.Second > maxTokenWaitTimeout {
			return pgtype.Timestamptz{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				fmt.Errorf("timeout_seconds must be between 1 and %d", int64(maxTokenWaitTimeout/time.Second))
		}
		duration := time.Duration(*request.TimeoutSeconds) * time.Second
		timeoutAt = pgvalue.Timestamptz(now.Add(duration))
		if duration <= checkpointDelay {
			checkpointDelay = duration + shortWaitGrace
		}
	}
	idleDuration := defaultTokenWaitIdleTimeout
	if request.IdleTimeoutSeconds != nil {
		idleDuration = time.Duration(*request.IdleTimeoutSeconds) * time.Second
		if *request.IdleTimeoutSeconds <= 0 || idleDuration > maxTokenWaitIdleTimeout {
			return pgtype.Timestamptz{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				fmt.Errorf("idle_timeout_seconds must be between 1 and %d", int64(maxTokenWaitIdleTimeout/time.Second))
		}
	}
	idleTimeout := pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true}
	if idleDuration < checkpointDelay {
		checkpointDelay = idleDuration
	}
	return timeoutAt, idleTimeout, pgvalue.Timestamptz(now.Add(checkpointDelay)), checkpointDelay, nil
}

func normalizeRunWaitPresentation(rawMetadata json.RawMessage, rawTags []string) (json.RawMessage, []string, error) {
	metadata := rawMetadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if len(metadata) > 64<<10 || !json.Valid(metadata) {
		return nil, nil, errors.New("metadata must be valid JSON no larger than 64 KiB")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &object); err != nil || object == nil {
		return nil, nil, errors.New("metadata must be a JSON object")
	}
	if len(rawTags) > 32 {
		return nil, nil, errors.New("tags must contain at most 32 values")
	}
	tags := append([]string(nil), rawTags...)
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" || tag != strings.TrimSpace(tag) || len(tag) > 128 {
			return nil, nil, errors.New("tags must be trimmed nonempty strings no larger than 128 bytes")
		}
	}
	canonical, err := canonicalJSON(metadata)
	if err != nil {
		return nil, nil, errors.New("metadata must be a canonicalizable JSON object")
	}
	return canonical, tags, nil
}

func tokenWaitDecision(state db.WaitState, result json.RawMessage, reason string) (string, json.RawMessage, error) {
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	switch state {
	case db.WaitStateCompleted:
		return "completed", result, nil
	case db.WaitStateCancelled:
		return "cancelled", result, nil
	case db.WaitStateFailed:
		if reason == "token_expired" {
			return "timed_out", result, nil
		}
		return "failed", json.RawMessage(fmt.Sprintf(`{"reason_code":%q}`, reason)), nil
	default:
		return "", nil, errors.New("Run Wait decision is not terminal")
	}
}

func decodeClosedWorkerRequest(r *http.Request, destination any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func decodeClosedJSON(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		return errors.New("value is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
