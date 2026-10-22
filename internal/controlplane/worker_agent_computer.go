package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"google.golang.org/protobuf/proto"
)

func agentComputerID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil() {
		return uuid.Nil(), session.ErrInvalidInput
	}
	return id, nil
}
func (server *Server) writeAgentComputerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workergroup.ErrStaleClaims):
		writeError(w, unauthorized(errors.New("worker authentication is stale")))
	case errors.Is(err, session.ErrInvalidInput), errors.Is(err, computer.ErrInvalidInput), errors.Is(err, dispatch.ErrInvalidInput):
		writeError(w, badRequest(errors.New("invalid Computer continuation request")))
	case errors.Is(err, session.ErrDenied), errors.Is(err, computer.ErrDenied), errors.Is(err, dispatch.ErrDenied):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerAuthorityUnavailable, message: "Computer continuation authority is unavailable"}))
	case errors.Is(err, session.ErrComputerCaptureDemand):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerCaptureDemand, message: "Computer has pending execution during capture"}))
	case errors.Is(err, session.ErrNotReady), errors.Is(err, computer.ErrNotReady), errors.Is(err, dispatch.ErrNotReady):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerNotReady, message: "Computer continuation requires reconciliation"}))
	case errors.Is(err, session.ErrConflict), errors.Is(err, computer.ErrConflict), errors.Is(err, dispatch.ErrConflict):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerEvidenceConflict, message: "Computer continuation evidence conflicts"}))
	default:
		server.log.Error("Computer continuation failed", "error", err)
		writeError(w, unavailable(errors.New("computer continuation is temporarily unavailable")))
	}
}

func (server *Server) workerBeginAgentComputerCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerCaptureRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	computer, e2 := agentComputerID(request.ComputerID)
	checkpoint, e3 := agentComputerID(request.CheckpointID)
	if e1 != nil || e2 != nil || e3 != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	result, err := session.BeginComputerCapture(r.Context(), server.tx, workerFromContext(r.Context()), session.ComputerCaptureRequest{EnvironmentID: env, ComputerID: computer, CheckpointID: checkpoint, LeaseEpoch: request.LeaseEpoch, ChannelCredential: request.ChannelCredential})
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerCaptureResponse{Capture: result.Request, SaveID: result.SaveID.String(), WorkPending: result.WorkPending, ReleaseSource: result.ReleaseSource})
}
func (server *Server) workerSealAgentComputerCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerReceiptRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	checkpoint, e2 := agentComputerID(request.CheckpointID)
	receipt := new(agentv1.ComputerSessionReceipt)
	if e1 != nil || e2 != nil || len(request.Receipt) == 0 || len(request.Receipt) > 64*1024 || proto.Unmarshal(request.Receipt, receipt) != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	if err := session.RecordComputerSealed(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerCancelAgentComputerCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerUnsealedRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	checkpoint, e2 := agentComputerID(request.CheckpointID)
	if e1 != nil || e2 != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	if err := session.CancelUnsealedComputerCapture(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, session.UnsealedCaptureEvidence{Rejected: request.Rejected, AbsentObservedAt: request.AbsentObservedAt}); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerAgentComputerSaveAbsence(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerSaveAbsenceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	checkpoint, e2 := agentComputerID(request.CheckpointID)
	if e1 != nil || e2 != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	if err := session.RecordCheckpointSaveAbsence(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.Evidence); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerPrepareAgentComputerRestore(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerRestoreRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	checkpoint, e2 := agentComputerID(request.CheckpointID)
	if e1 != nil || e2 != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	p, err := session.PrepareComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.LeaseEpoch, request.ChannelCredential)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerInstallationResponse{Installation: raw})
}

func decodeAgentComputerInstallation(request workerapi.AgentComputerInstallationRequest) (uuid.UUID, *agentv1.ComputerSessionInstallation, *agentv1.ComputerSessionReceipt, error) {
	env, err := agentComputerID(request.EnvironmentID)
	p, receipt := new(agentv1.ComputerSessionInstallation), new(agentv1.ComputerSessionReceipt)
	if err != nil || len(request.Installation) == 0 || len(request.Installation) > 16*1024*1024 || len(request.Receipt) == 0 || len(request.Receipt) > 64*1024 {
		return env, nil, nil, session.ErrInvalidInput
	}
	if proto.Unmarshal(request.Installation, p) != nil || proto.Unmarshal(request.Receipt, receipt) != nil {
		return env, nil, nil, session.ErrInvalidInput
	}
	return env, p, receipt, nil
}

func (server *Server) workerValidateAgentComputerSourceAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.ValidateComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerCommitAgentComputerSourceAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.CommitComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerCompleteAgentComputerSourceAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.CompleteComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerValidateAgentComputerRestore(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.ValidateComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerCommitAgentComputerRestore(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.CommitComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerCompleteAgentComputerRestore(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	if err := session.CompleteComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerAgentComputerControls(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerInstallationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, p, receipt, err := decodeAgentComputerInstallation(request)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	controls, err := session.ReadComputerContinuationControls(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(controls)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerControlsResponse{Controls: raw})
}

func (server *Server) workerPrepareAgentComputerSourceAbort(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentComputerSourceAbortRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	env, e1 := agentComputerID(request.EnvironmentID)
	checkpoint, e2 := agentComputerID(request.CheckpointID)
	receipt := new(agentv1.ComputerSessionReceipt)
	if e1 != nil || e2 != nil || len(request.Receipt) == 0 || len(request.Receipt) > 64*1024 || proto.Unmarshal(request.Receipt, receipt) != nil {
		server.writeAgentComputerError(w, session.ErrInvalidInput)
		return
	}
	p, err := session.PrepareComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.LeaseEpoch, request.ChannelCredential, receipt)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerInstallationResponse{Installation: raw})
}
