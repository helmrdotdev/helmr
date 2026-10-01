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
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
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
	parsed, worker, registrationLocators, current, err := s.loadRunWaitRegistrationAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	idleDefault, err := s.runWaitIdleDefault(r.Context(), current)
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
	fingerprint, err := run.RequestFingerprint("worker.run-wait.create.v1", normalized)
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

	registered, err := run.RegisterTimerWait(r.Context(), s.tx, run.TimerWait{
		Fence:  workerExecutionFence(worker, parsed, request.Lease),
		WaitID: waitID, TurnID: request.TurnID, RunGeneration: request.RunGeneration,
		Cursor: actorCursor, Fingerprint: fingerprint,
		DueAt: dueAt, IdleTimeout: idleTimeout, Metadata: metadata, Tags: tags,
	})
	if err != nil {
		mapped := runError(err, runTimerWaitOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("register worker timer Wait failed", "run_id", pgvalue.UUIDString(registrationLocators.RunID), "error", err)
		}
		writeError(w, mapped)
		return
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(registrationLocators.RunID), RunWaitID: waitID.String(),
		ResumeAttachID:     resumeAttachID.String(),
		ComputerInstanceID: pgvalue.UUIDString(registrationLocators.ComputerInstanceID),
		WorkerEpoch:        worker.Epoch,
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
