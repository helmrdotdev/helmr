package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
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
	result, err := session.ControlComputerExecution(r.Context(), s.auditPolicy, s.tx, caller, session.ComputerExecutionRequest{EnvironmentID: env, ComputerID: id, Kind: kind, IdempotencyKey: key})
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

// computerHibernatePreviewHTTP reports how many open Sessions a hibernate
// request would pause.
func (s *Server) computerHibernatePreviewHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionComputersHibernate)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "computerID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	var count int64
	err = db.RunTx(r.Context(), s.tx, func(tx pgx.Tx) error {
		count, err = session.ComputerHibernatePreview(r.Context(), tx, caller, env, id)
		return err
	})
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ComputerHibernatePreview{Sessions: count})
}

// sessionClosePreviewHTTP reports how many Sessions a close would close.
func (s *Server) sessionClosePreviewHTTP(w http.ResponseWriter, r *http.Request) {
	caller, env, err := s.agentRequestAuthority(r, auth.PermissionSessionsClose)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	id, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	var count int64
	err = db.RunTx(r.Context(), s.tx, func(tx pgx.Tx) error {
		count, err = session.SessionClosePreview(r.Context(), tx, caller, env, id)
		return err
	})
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SessionClosePreview{Sessions: count})
}
