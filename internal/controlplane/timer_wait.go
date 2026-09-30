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
	"github.com/helmrdotdev/helmr/internal/session"
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
	metadata, tags, err := normalizeWaitAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, registrationLocators, run, err := s.loadRunWaitRegistrationAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	idleDefault, err := s.runWaitIdleDefault(r.Context(), run)
	if err != nil {
		writeError(w, err)
		return
	}
	params, dueAt, idleTimeout, err := timerWaitDeadlines(request, idleDefault)
	if err != nil {
		writeError(w, badRequest(err))
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
		authority, err := lockWorkerWaitExecution(r.Context(), work.tx, worker, parsed, request.Lease)
		if err != nil {
			return err
		}
		turnID, generation, err := parseWorkerWaitTurn(request.TurnID, request.RunGeneration)
		if err != nil {
			return err
		}
		if err := session.ValidateWaitCursor(authority, db.RunWait{TurnID: turnID, TurnRunGeneration: generation, TurnSessionID: authority.Session.ID}, actorCursor); err != nil {
			return err
		}
		if turnID.Valid {
			if _, err := session.ValidateTurnWork(r.Context(), work.q, session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.Run.EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.Session.ID), RunID: pgvalue.MustUUIDValue(authority.Run.ID), TurnID: pgvalue.MustUUIDValue(turnID), AttemptNumber: authority.Attempt.Number, RunGeneration: generation.Int64}); err != nil {
				return err
			}
		}
		registered, err = work.q.GetTimerRunWaitRegistrationReplay(
			r.Context(),
			db.GetTimerRunWaitRegistrationReplayParams{
				ID: pgvalue.UUID(waitID), EnvironmentID: authority.Run.EnvironmentID,
				RunID: authority.Run.ID, ComputerID: authority.Computer.ID,
				AttemptNumber:                  authority.Attempt.Number,
				RegistrationRequestFingerprint: pgvalue.Text(fingerprint),
				Metadata:                       metadata, Tags: tags, RunLeaseID: authority.Lease.ID,
			},
		)
		if err == nil {
			return session.ValidateWaitCursor(authority, registered, actorCursor)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, existingErr := work.q.GetRunWait(r.Context(), db.GetRunWaitParams{
			RunID: authority.Run.ID, AttemptNumber: authority.Attempt.Number,
			ID: pgvalue.UUID(waitID),
		}); existingErr == nil || !errors.Is(existingErr, pgx.ErrNoRows) {
			return errStaleRunLeaseClaim
		}
		if authority.Run.Status != db.RunStatusRunning {
			return errStaleRunLeaseClaim
		}
		registered, err = work.q.RegisterTimerRunWait(r.Context(), db.RegisterTimerRunWaitParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.Run.EnvironmentID,
			DueAt: pgvalue.Timestamptz(dueAt), IdleTimeoutMs: idleTimeout,
			RegistrationRequestFingerprint: pgvalue.Text(fingerprint),
			AttemptNumber:                  authority.Attempt.Number,
			CurrentRunLeaseID:              authority.Lease.ID,
			Metadata:                       metadata, Tags: tags,
			RunID:                   authority.Run.ID,
			ExpectedRunningRevision: authority.Run.Revision,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if turnID.Valid {
			_, err = work.q.BindRunWaitTurn(r.Context(), db.BindRunWaitTurnParams{SessionID: authority.Session.ID, TurnID: turnID, RunGeneration: generation, WaitID: registered.ID})
		}
		return err
	})
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, session.ErrAuthority) || errors.Is(err, session.ErrTurnStopped) || errors.Is(err, session.ErrTurnScope) {
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
		ResumeAttachID:     resumeAttachID.String(),
		ComputerInstanceID: pgvalue.UUIDString(registrationLocators.ComputerInstanceID),
		RuntimeEpoch:       worker.Epoch,
	}
	if registered.SuspensionStatus == db.RunWaitStatusReleased {
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
	defaultIdleTimeout time.Duration,
) (workerTimerWaitParams, time.Time, pgtype.Int8, error) {
	var params workerTimerWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		return params, time.Time{}, pgtype.Int8{},
			fmt.Errorf("invalid timer wait params: %w", err)
	}
	if (params.Duration == nil) == (params.Date == nil) {
		return params, time.Time{}, pgtype.Int8{},
			errors.New("timer wait params must contain exactly one of duration or date")
	}
	if request.TimeoutMS == nil || *request.TimeoutMS <= 0 ||
		*request.TimeoutMS > maxRunWaitDuration.Milliseconds() {
		return params, time.Time{}, pgtype.Int8{},
			fmt.Errorf("timeout_ms must be between 1 and %d", maxRunWaitDuration.Milliseconds())
	}
	now := time.Now().UTC()
	var dueAt time.Time
	if params.Duration != nil {
		duration, err := parseTimerDuration(*params.Duration)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{}, err
		}
		if duration.Milliseconds() != *request.TimeoutMS {
			return params, time.Time{}, pgtype.Int8{},
				errors.New("timer duration and timeout_ms must match")
		}
		dueAt = now.Add(duration)
	} else {
		parsed, err := time.Parse(time.RFC3339Nano, *params.Date)
		if err != nil {
			return params, time.Time{}, pgtype.Int8{},
				errors.New("timer date must be an RFC3339 timestamp")
		}
		dueAt = parsed.UTC()
		normalized := dueAt.Format(time.RFC3339Nano)
		params.Date = &normalized
		if dueAt.After(now.Add(maxRunWaitDuration)) {
			return params, time.Time{}, pgtype.Int8{},
				errors.New("timer date must not be more than 365d in the future")
		}
	}
	idleDuration, err := runWaitIdleDuration(request.IdleTimeoutMS, defaultIdleTimeout)
	if err != nil {
		return params, time.Time{}, pgtype.Int8{}, err
	}
	return params, dueAt,
		pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true}, nil
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
	if wait.Kind != db.WaitKindTimer || wait.ConditionStatus != db.WaitStatusCompleted {
		return "", nil, errors.New("timer wait decision is not completed")
	}
	return "completed", json.RawMessage(`null`), nil
}
