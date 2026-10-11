package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"google.golang.org/protobuf/proto"
)

func agentComputerID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil() {
		return uuid.Nil(), agent.ErrInvalidInput
	}
	return id, nil
}
func (server *Server) writeAgentComputerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workergroup.ErrStaleClaims):
		writeError(w, unauthorized(errors.New("worker authentication is stale")))
	case errors.Is(err, agent.ErrInvalidInput):
		writeError(w, badRequest(errors.New("invalid Computer continuation request")))
	case errors.Is(err, agent.ErrDenied):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerAuthorityUnavailable, message: "Computer continuation authority is unavailable"}))
	case errors.Is(err, agent.ErrNotReady):
		writeError(w, conflict(codedError{code: workerapi.AgentComputerNotReady, message: "Computer continuation requires reconciliation"}))
	case errors.Is(err, agent.ErrConflict):
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	result, err := agent.BeginComputerCapture(r.Context(), server.tx, workerFromContext(r.Context()), agent.ComputerCaptureRequest{EnvironmentID: env, ComputerID: computer, CheckpointID: checkpoint, LeaseEpoch: request.LeaseEpoch, ChannelCredential: request.ChannelCredential})
	if err != nil {
		server.writeAgentComputerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentComputerCaptureResponse{Capture: result.Request, SaveID: result.SaveID.String()})
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	if err := agent.RecordComputerSealed(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, receipt); err != nil {
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	if err := agent.CancelUnsealedComputerCapture(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, agent.UnsealedCaptureEvidence{Rejected: request.Rejected, AbsentObservedAt: request.AbsentObservedAt}); err != nil {
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	if err := agent.RecordCheckpointSaveAbsence(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.Evidence); err != nil {
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	p, err := agent.PrepareComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.LeaseEpoch, request.ChannelCredential)
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
		return env, nil, nil, agent.ErrInvalidInput
	}
	if proto.Unmarshal(request.Installation, p) != nil || proto.Unmarshal(request.Receipt, receipt) != nil {
		return env, nil, nil, agent.ErrInvalidInput
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
	if err := agent.ValidateComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	if err := agent.CommitComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	if err := agent.CompleteComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	if err := agent.ValidateComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	if err := agent.CommitComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	if err := agent.CompleteComputerRestore(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt); err != nil {
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
	controls, err := agent.ReadComputerContinuationControls(r.Context(), server.tx, workerFromContext(r.Context()), env, p, receipt)
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
		server.writeAgentComputerError(w, agent.ErrInvalidInput)
		return
	}
	p, err := agent.PrepareComputerSourceAbort(r.Context(), server.tx, workerFromContext(r.Context()), env, checkpoint, request.LeaseEpoch, request.ChannelCredential, receipt)
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
