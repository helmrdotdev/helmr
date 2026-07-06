package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type workerTokenWaitParams struct {
	TokenID string `json:"token_id"`
}

func (s *Server) matchImmediateWorkerTokenWait(ctx context.Context, scope db.GetWorkerRunWaitScopeRow, request api.WorkerCreateRunWaitRequest) (string, json.RawMessage, bool, error) {
	tokenID, err := workerTokenWaitTokenID(request)
	if err != nil {
		return "", nil, false, err
	}
	token, err := s.db.GetToken(ctx, db.GetTokenParams{
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            pgvalue.UUID(tokenID),
	})
	if isNoRows(err) {
		return "", nil, false, errTokenNotFound
	}
	if err != nil {
		return "", nil, false, err
	}
	return workerTokenResolution(token)
}

func workerTokenWaitTokenID(request api.WorkerCreateRunWaitRequest) (uuid.UUID, error) {
	var params workerTokenWaitParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return uuid.UUID{}, badRequest(fmt.Errorf("invalid token wait params: %w", err))
	}
	tokenID, err := uuid.Parse(strings.TrimSpace(params.TokenID))
	if err != nil {
		return uuid.UUID{}, badRequest(errors.New("token wait params.token_id must be a UUID"))
	}
	return tokenID, nil
}

func (s *Server) workerCreateToken(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCreateTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker token request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	scope, err := s.db.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(orgID),
		RunID:            pgvalue.UUID(runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker token scope"))
		return
	}
	timeout := json.RawMessage(`"7d"`)
	if request.TimeoutAt != nil {
		b, _ := json.Marshal(map[string]string{"date": request.TimeoutAt.UTC().Format(time.RFC3339Nano)})
		timeout = b
	} else if request.TimeoutInSeconds != nil {
		b, _ := json.Marshal(map[string]int32{"seconds": *request.TimeoutInSeconds})
		timeout = b
	}
	token, publicToken, err := s.createTokenRecord(r.Context(), s.db, auth.Actor{OrgID: orgID}, scope.ProjectID, scope.EnvironmentID, api.CreateTokenRequest{
		Timeout:  timeout,
		Tags:     request.Tags,
		Metadata: request.Metadata,
	})
	if err != nil {
		s.writeWorkerWaitError(w, err)
		return
	}
	row := tokenFromCreateRow(token)
	writeJSON(w, http.StatusOK, api.TokenResponse{
		ID:                pgvalue.MustUUIDValue(row.ID).String(),
		Status:            string(row.State),
		CallbackURL:       s.tokenCallbackURL(pgvalue.MustUUIDValue(row.ID)),
		PublicAccessToken: publicToken,
		TimeoutAt:         &row.TimeoutAt.Time,
		Tags:              row.Tags,
		Metadata:          json.RawMessage(row.Metadata),
	})
}
