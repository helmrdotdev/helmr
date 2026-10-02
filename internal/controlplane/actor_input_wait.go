package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type workerSessionInputWaitParams struct {
	SessionID          string `json:"session_id"`
	AfterInputSequence int64  `json:"after_input_sequence"`
}

type actorWaitManifest struct {
	IdleTimeoutMS int64 `json:"idleTimeoutMs"`
}

func (s *Server) workerCreateSessionInputRunWait(
	w http.ResponseWriter,
	r *http.Request,
	request workerapi.CreateRunWaitRequest,
	identity requestedRunWaitIdentity,
) {
	var params workerSessionInputWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid actor input wait params: %w", err)))
		return
	}
	sessionID, err := parseCanonicalUUID("params.session_id", params.SessionID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if params.AfterInputSequence < 0 || request.SessionSpeculativeInputSequence == nil ||
		params.AfterInputSequence != *request.SessionSpeculativeInputSequence {
		writeError(w, badRequest(errors.New("actor input wait cursors must be present, non-negative, and equal")))
		return
	}
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
	if current.EntrypointKind != "actor" || !current.SessionID.Valid || current.SessionID != pgvalue.UUID(sessionID) {
		writeError(w, conflict(errors.New("actor input wait must target the owning actor")))
		return
	}
	idleTimeoutDefault, err := s.runWaitIdleDefault(r.Context(), current)
	if err != nil {
		writeError(w, err)
		return
	}
	timeoutAt, idleTimeout, err := runWaitDeadlines(
		request, idleTimeoutDefault,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	normalized := request
	normalized.Params, err = json.Marshal(workerSessionInputWaitParams{
		SessionID: sessionID.String(), AfterInputSequence: params.AfterInputSequence,
	})
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize actor input wait params: %w", err)))
		return
	}
	normalized.Metadata = metadata
	normalized.Tags = tags
	fingerprint, err := run.RequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint actor input wait registration: %w", err)))
		return
	}
	waitID := identity.waitID
	resumeAttachID := identity.resumeAttachID

	registered, err := session.RegisterInputWait(r.Context(), s.tx, workerExecutionFence(worker, parsed, request.Lease), session.InputWait{
		WaitID: waitID, SessionID: sessionID, AfterInputSequence: params.AfterInputSequence,
		Fingerprint: fingerprint, Metadata: metadata, Tags: tags, TimeoutAt: timeoutAt, IdleTimeout: idleTimeout,
	})
	if err != nil {
		mapped := sessionError(err, sessionWorkerWaitOperation)
		if errorStatus(mapped) == http.StatusInternalServerError {
			s.log.Error("register worker Actor input Wait failed", "run_id", pgvalue.UUIDString(registrationLocators.RunID), "error", err)
		}
		writeError(w, mapped)
		return
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(registrationLocators.RunID), RunWaitID: waitID.String(), ResumeAttachID: resumeAttachID.String(),
		ComputerInstanceID: pgvalue.UUIDString(registrationLocators.ComputerInstanceID), WorkerEpoch: worker.Epoch,
	}
	if registered.SuspensionStatus == db.RunWaitStatusReleased {
		response.ResolutionKind, response.Resolution, err = sessionInputWaitDecision(registered)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	s.captureDrainingComputers(r.Context(), worker.HostID, pgvalue.MustUUIDValue(registrationLocators.ComputerInstanceID))
	writeJSON(w, http.StatusOK, response)
}

func actorWaitIdleTimeout(raw json.RawMessage) (time.Duration, error) {
	var manifest actorWaitManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, err
	}
	if manifest.IdleTimeoutMS <= 0 || manifest.IdleTimeoutMS > maxRunWaitIdleTimeout.Milliseconds() {
		return 0, errors.New("actor idle timeout is outside the supported range")
	}
	return time.Duration(manifest.IdleTimeoutMS) * time.Millisecond, nil
}

func sessionInputWaitDecision(wait db.RunWait) (string, json.RawMessage, error) {
	switch wait.ConditionStatus {
	case db.WaitStatusCompleted:
		if !wait.CompletedTurnID.Valid || len(wait.ConditionResult) == 0 {
			return "", nil, errors.New("completed actor input wait is missing its record")
		}
		return "completed", wait.ConditionResult, nil
	case db.WaitStatusFailed:
		reason := pgvalue.TextValue(wait.ConditionReasonCode)
		if reason != "wait_timeout" && reason != "session_closed" {
			return "", nil, errors.New("actor input wait failure reason is invalid")
		}
		payload, _ := json.Marshal(map[string]string{"reason_code": reason})
		return "failed", payload, nil
	case db.WaitStatusCancelled:
		payload, _ := json.Marshal(map[string]string{"reason_code": pgvalue.TextValue(wait.ConditionReasonCode)})
		return "cancelled", payload, nil
	default:
		return "", nil, errors.New("actor input wait is not terminal")
	}
}
