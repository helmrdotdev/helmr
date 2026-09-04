package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	request workerapi.CreateRunWaitRequest,
	identity requestedRunWaitIdentity,
) {
	params, dueAt, idleTimeout, checkpointDueAt, err := timerWaitDeadlines(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, tags, err := normalizeWaitAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, registrationLocators, _, err := s.loadRunWaitRegistrationAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	normalized := request
	normalized.Params, err = json.Marshal(params)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize timer wait params: %w", err)))
		return
	}
	normalized.Metadata = metadata
	normalized.Tags = tags
	fingerprint, err := terminalRequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint timer wait registration: %w", err)))
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
		locators, err := work.q.GetLiveRunLeaseLocators(r.Context(), db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID:    pgvalue.UUID(worker.WorkerGroupID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := secret.LockAttemptDelivery(
			r.Context(), work.q, locators.RunID, locators.AttemptNumber,
			locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock timer wait secret authority: %w", err)
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
		writeError(w, conflict(errors.New("worker timer wait receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("register worker timer Wait failed", "run_id", pgvalue.UUIDString(registrationLocators.RunID), "error", err)
		writeError(w, errors.New("register worker timer wait"))
		return
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(registrationLocators.RunID), RunWaitID: waitID.String(),
		ResumeAttachID:    resumeAttachID.String(),
		RuntimeInstanceID: pgvalue.UUIDString(registrationLocators.RuntimeInstanceID),
		RuntimeEpoch:      worker.WorkerEpoch,
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
	request workerapi.CreateRunWaitRequest,
) (workerTimerWaitParams, time.Time, pgtype.Int8, pgtype.Timestamptz, error) {
	var params workerTimerWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
			fmt.Errorf("invalid timer wait params: %w", err)
	}
	if (params.Duration == nil) == (params.Date == nil) {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
			errors.New("timer wait params must contain exactly one of duration or date")
	}
	if request.TimeoutMS == nil || *request.TimeoutMS <= 0 ||
		*request.TimeoutMS > maxRunWaitDuration.Milliseconds() {
		return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
			fmt.Errorf("timeout_ms must be between 1 and %d", maxRunWaitDuration.Milliseconds())
	}
	now := time.Now().UTC()
	var dueAt time.Time
	if params.Duration != nil {
		duration, err := parseTimerDuration(*params.Duration)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{}, err
		}
		if duration.Milliseconds() != *request.TimeoutMS {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
				errors.New("timer duration and timeout_ms must match")
		}
		dueAt = now.Add(duration)
	} else {
		parsed, err := time.Parse(time.RFC3339Nano, *params.Date)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
				errors.New("timer date must be an RFC3339 timestamp")
		}
		dueAt = parsed.UTC()
		normalized := dueAt.Format(time.RFC3339Nano)
		params.Date = &normalized
		if dueAt.After(now.Add(maxRunWaitDuration)) {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
				errors.New("timer date must not be more than 365d in the future")
		}
	}
	idleDuration := defaultRunWaitIdleTimeout
	if request.IdleTimeoutMS != nil {
		if *request.IdleTimeoutMS <= 0 || *request.IdleTimeoutMS > maxRunWaitIdleTimeout.Milliseconds() {
			return params, time.Time{}, pgtype.Int8{}, pgtype.Timestamptz{},
				fmt.Errorf("idle_timeout_ms must be between 1 and %d", maxRunWaitIdleTimeout.Milliseconds())
		}
		idleDuration = time.Duration(*request.IdleTimeoutMS) * time.Millisecond
	}
	checkpointDelay := rootRunWaitHotWindow
	untilDue := dueAt.Sub(now)
	if untilDue <= checkpointDelay {
		checkpointDelay = max(untilDue+shortWaitGrace, shortWaitGrace)
	}
	if idleDuration < checkpointDelay {
		checkpointDelay = idleDuration
	}
	return params, dueAt,
		pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true},
		pgvalue.Timestamptz(now.Add(checkpointDelay)), nil
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
		return "", nil, errors.New("timer wait decision is not completed")
	}
	return "completed", json.RawMessage(`null`), nil
}
