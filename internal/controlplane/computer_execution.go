package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
)

func (s *Server) hibernateComputerHTTP(w http.ResponseWriter, r *http.Request) {
	s.controlComputerExecutionHTTP(w, r, "hibernate")
}
func (s *Server) wakeComputerHTTP(w http.ResponseWriter, r *http.Request) {
	s.controlComputerExecutionHTTP(w, r, "wake")
}

func (s *Server) controlComputerExecutionHTTP(w http.ResponseWriter, r *http.Request, kind string) {
	permission := auth.PermissionComputersWake
	if kind == "hibernate" {
		permission = auth.PermissionComputersHibernate
	}
	caller, env, err := s.agentRequestAuthority(r, permission)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_computer_reference", message: "computer ID is invalid"}))
		return
	}
	var body struct {
		IdempotencyKey string `json:"idempotency_key"`
		ExpectedScope  string `json:"expected_scope,omitempty"`
	}
	if err := decodeOptionalRequestJSON(r, &body); err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	key, err := normalizeIdempotencyKey(body.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(codedError{code: "invalid_idempotency_key", message: err.Error()}))
		return
	}
	if key == "" {
		key = uuid.NewV7().String()
	}
	result, err := session.ControlComputerExecution(r.Context(), s.auditPolicy, s.tx, caller, session.ComputerExecutionRequest{EnvironmentID: env, ComputerID: id, Kind: kind, IdempotencyKey: key, ExpectedScope: body.ExpectedScope})
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNotReady), errors.Is(err, computer.ErrNotReady):
			s.writeOperationError(w, r, conflict(codedError{code: "computer_execution_unavailable", message: "Computer cannot change execution state right now. Review its current status before trying again."}))
		case errors.Is(err, computer.ErrBusy):
			s.writeOperationError(w, r, conflict(codedError{code: "computer_busy", message: "Computer has active work or an unfinished control request"}))
		case errors.Is(err, computer.ErrDeleting):
			s.writeOperationError(w, r, conflict(codedError{code: "computer_deleting", message: "Computer is being deleted"}))
		default:
			s.writeAgentHTTPError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) computerExecutionScopeHTTP(w http.ResponseWriter, r *http.Request) {
	s.executionScopeHTTP(w, r, false)
}
func (s *Server) sessionCloseScopeHTTP(w http.ResponseWriter, r *http.Request) {
	s.executionScopeHTTP(w, r, true)
}
func (s *Server) executionScopeHTTP(w http.ResponseWriter, r *http.Request, close bool) {
	permission, param := auth.PermissionComputersRead, "computerID"
	if close {
		permission, param = auth.PermissionSessionsClose, "sessionID"
	}
	caller, env, err := s.agentRequestAuthority(r, permission)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, param))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	var scope session.ExecutionScope
	err = db.RunTx(r.Context(), s.tx, func(tx pgx.Tx) error {
		if close {
			scope, err = session.SessionCloseScope(r.Context(), tx, caller, env, id)
			return err
		}
		var exists bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM computers WHERE environment_id=$1 AND id=$2)`, env, id).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return session.ErrNotReady
		}
		scope, err = session.ReadComputerExecutionScope(r.Context(), tx, env, id)
		return err
	})
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, scope)
}
