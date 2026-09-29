package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"net/http"
)

func cancelCommandInTx(ctx context.Context, work *txWork, scope db.GetCommandParams) (api.CommandCancelReceipt, error) {
	var receipt api.CommandCancelReceipt
	_, err := work.q.GetCommand(ctx, scope)
	if err != nil {
		return receipt, err
	}
	request, err := idempotency.NewCommandCancelRequest(pgvalue.MustUUIDValue(scope.EnvironmentID), pgvalue.MustUUIDValue(scope.CommandID))
	if err != nil {
		return receipt, err
	}
	claims, err := idempotency.TransactionFor(work.tx)
	if err != nil {
		return receipt, err
	}
	acquired, err := claims.Acquire(ctx, request)
	if err != nil {
		return receipt, err
	}
	if !acquired.New {
		err = json.Unmarshal(acquired.Claim.Receipt, &receipt)
		if err == nil && (receipt.ID != pgvalue.UUIDString(acquired.Claim.ID) || receipt.TargetID != pgvalue.UUIDString(scope.CommandID) || receipt.Status != "accepted") {
			err = errors.New("Command cancellation receipt is invalid")
		}
		return receipt, err
	}
	_, err = work.q.RequestComputerCommandCancellation(ctx, db.RequestComputerCommandCancellationParams{EnvironmentID: scope.EnvironmentID, CommandID: scope.CommandID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	receipt = api.CommandCancelReceipt{ID: pgvalue.UUIDString(acquired.Claim.ID), TargetID: pgvalue.UUIDString(scope.CommandID), Status: "accepted"}
	body, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	_, err = claims.Complete(ctx, acquired.Claim, body)
	return receipt, err
}

func (s *Server) cancelCommandHTTP(w http.ResponseWriter, r *http.Request) {
	commandID, err := ids.Parse(chi.URLParam(r, "commandID"))
	if err != nil {
		writeError(w, badRequest(errors.New("command ID is invalid")))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	var receipt api.CommandCancelReceipt
	err = s.inTx(r.Context(), func(work *txWork) error {
		var err error
		receipt, err = cancelCommandInTx(r.Context(), work, db.GetCommandParams{OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID, CommandID: pgvalue.UUID(commandID)})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "computer_command_not_found", message: "Command was not found"}))
		return
	}
	if err != nil {
		s.writeComputerCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
}
