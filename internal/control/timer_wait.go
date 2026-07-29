package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var timerDurationPattern = regexp.MustCompile(`^([1-9][0-9]*)(ms|s|m|h|d)$`)

type workerTimerWaitParams struct {
	Duration *string `json:"duration,omitempty"`
	Date     *string `json:"date,omitempty"`
}

func (s *Server) workerCreateTimerRunWait(
	w http.ResponseWriter,
	r *http.Request,
	request api.WorkerCreateRunWaitRequest,
	identity requestedRunWaitIdentity,
) {
	params, dueAt, idleTimeout, checkpointDueAt, checkpointDelay, err := timerWaitDeadlines(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, tags, err := normalizeWaitAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, _, _, err := s.loadRunWaitRegistrationAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.Params, err = json.Marshal(params)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize timer Wait params: %w", err)))
		return
	}
	normalized.Metadata = metadata
	normalized.Tags = tags
	fingerprint, err := terminalRequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint timer Wait registration: %w", err)))
		return
	}
	waitID := identity.waitID
	resumeAttachID := identity.resumeAttachID
	actorCursor := pgtype.Int8{}
	if request.ActorSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{Int64: *request.ActorSpeculativeInputSequence, Valid: true}
	}

	var registered db.RunWait
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(
			r.Context(), work.q, pgvalue.UUID(parsed.runID), request.Lease.AttemptNumber,
			pgvalue.UUID(parsed.workspaceID),
		); err != nil {
			return fmt.Errorf("lock timer Wait Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(r.Context(), db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		owner, err := lockRunFinalizationOwner(r.Context(), work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			r.Context(), work.q, worker, pgvalue.UUID(parsed.leaseID),
			request.Lease.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.runLease.State != db.RunLeaseStateRunning {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease,
			workspace: authority.workspace, workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil ||
			!equalCurrentOrPreviousRunLeaseReceipt(current, request.Lease, authority.runLease.PreviousExpiresAt) {
			return errStaleRunLeaseClaim
		}
		if err := validateRunWaitActorCursor(authority, db.RunWait{
			ActorSpeculativeInputSequence: actorCursor,
		}); err != nil {
			return err
		}
		registered, err = work.q.GetTimerRunWaitRegistrationReplay(
			r.Context(),
			db.GetTimerRunWaitRegistrationReplayParams{
				ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID,
				RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber:                  authority.attempt.Number,
				ActorSpeculativeInputSequence:  actorCursor,
				ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
				RegistrationRequestFingerprint: pgvalue.Text(fingerprint),
				Metadata:                       metadata, Tags: tags, RunLeaseID: authority.runLease.ID,
			},
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, existingErr := work.q.GetRunWait(r.Context(), db.GetRunWaitParams{
			RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			ID: pgvalue.UUID(waitID),
		}); existingErr == nil || !errors.Is(existingErr, pgx.ErrNoRows) {
			return errStaleRunLeaseClaim
		}
		if authority.run.Status != db.RunStatusRunning {
			return errStaleRunLeaseClaim
		}
		registered, err = work.q.RegisterTimerRunWait(r.Context(), db.RegisterTimerRunWaitParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID,
			DueAt: pgvalue.Timestamptz(dueAt), IdleTimeoutMs: idleTimeout,
			RegistrationRequestFingerprint: pgvalue.Text(fingerprint),
			AttemptNumber:                  authority.attempt.Number,
			ActorSpeculativeInputSequence:  actorCursor,
			CurrentRunLeaseID:              authority.runLease.ID,
			CheckpointDueAt:                checkpointDueAt,
			ResumeAttachID:                 pgvalue.UUID(resumeAttachID),
			Metadata:                       metadata, Tags: tags,
			RunID:                       authority.run.ID,
			ExpectedRunningStateVersion: authority.run.StateVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		return nil
	})
	if errors.Is(err, errStaleRunLeaseClaim) {
		writeError(w, conflict(errors.New("worker timer Wait receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("register worker timer Wait failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("register worker timer Wait"))
		return
	}
	response := api.WorkerCreateRunWaitResponse{
		RunID: request.Lease.RunID, RunWaitID: waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: request.Lease.RuntimeInstanceID,
		RuntimeEpoch:      request.Lease.WorkerEpoch,
		CheckpointDelayMs: checkpointDelay.Milliseconds(),
	}
	if registered.SuspensionState == db.RunWaitStateReleased {
		response.ResolutionKind, response.Resolution, err = timerWaitDecision(registered)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func timerWaitDeadlines(
	request api.WorkerCreateRunWaitRequest,
) (workerTimerWaitParams, time.Time, pgtype.Int8, pgtype.Timestamptz, time.Duration, error) {
	var params workerTimerWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
			fmt.Errorf("invalid timer Wait params: %w", err)
	}
	if (params.Duration == nil) == (params.Date == nil) {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
			errors.New("timer Wait params must contain exactly one of duration or date")
	}
	if request.TimeoutMS == nil || *request.TimeoutMS <= 0 ||
		*request.TimeoutMS > maxRunWaitDuration.Milliseconds() {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
			fmt.Errorf("timeout_ms must be between 1 and %d", maxRunWaitDuration.Milliseconds())
	}
	now := time.Now().UTC()
	var dueAt time.Time
	if params.Duration != nil {
		duration, err := parseTimerDuration(*params.Duration)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0, err
		}
		if duration.Milliseconds() != *request.TimeoutMS {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				errors.New("timer duration and timeout_ms must match")
		}
		dueAt = now.Add(duration)
	} else {
		parsed, err := time.Parse(time.RFC3339Nano, *params.Date)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				errors.New("timer date must be an RFC3339 timestamp")
		}
		dueAt = parsed.UTC()
		normalized := dueAt.Format(time.RFC3339Nano)
		params.Date = &normalized
		if dueAt.After(now.Add(maxRunWaitDuration)) {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				errors.New("timer date must not be more than 365d in the future")
		}
	}
	idleDuration := defaultRunWaitIdleTimeout
	if request.IdleTimeoutMS != nil {
		if *request.IdleTimeoutMS <= 0 || *request.IdleTimeoutMS > maxRunWaitIdleTimeout.Milliseconds() {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, 0,
				fmt.Errorf("idle_timeout_ms must be between 1 and %d", maxRunWaitIdleTimeout.Milliseconds())
		}
		idleDuration = time.Duration(*request.IdleTimeoutMS) * time.Millisecond
	}
	checkpointDelay := rootRunWaitHotWindow
	untilDue := dueAt.Sub(now)
	if untilDue <= checkpointDelay {
		checkpointDelay = untilDue + shortWaitGrace
		if checkpointDelay < shortWaitGrace {
			checkpointDelay = shortWaitGrace
		}
	}
	if idleDuration < checkpointDelay {
		checkpointDelay = idleDuration
	}
	return params, dueAt,
		pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true},
		pgvalue.Timestamptz(now.Add(checkpointDelay)), checkpointDelay, nil
}

func parseTimerDuration(value string) (time.Duration, error) {
	match := timerDurationPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("timer duration must be a positive integer followed by ms, s, m, h, or d")
	}
	amount, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, errors.New("timer duration is outside the supported range")
	}
	multiplier := time.Millisecond
	switch match[2] {
	case "s":
		multiplier = time.Second
	case "m":
		multiplier = time.Minute
	case "h":
		multiplier = time.Hour
	case "d":
		multiplier = 24 * time.Hour
	}
	if amount > int64(maxRunWaitDuration/multiplier) {
		return 0, errors.New("timer duration must be between 1ms and 365d")
	}
	return time.Duration(amount) * multiplier, nil
}

func timerWaitDecision(wait db.RunWait) (string, json.RawMessage, error) {
	if wait.Kind != db.WaitKindTimer || wait.ConditionState != db.WaitStateCompleted {
		return "", nil, errors.New("timer Wait decision is not completed")
	}
	return "completed", json.RawMessage(`null`), nil
}
