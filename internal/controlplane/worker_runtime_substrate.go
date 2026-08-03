package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRegisterRuntimeSubstrate(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RuntimeSubstrateRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime substrate register request JSON: %w", err)))
		return
	}
	if err := validateRuntimeSubstrateRegisterRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	workspaceDefinitionID, err := ids.Parse(request.DeploymentDefinitionID)
	if err != nil {
		writeError(w, badRequest(errors.New("deployment_definition_id must be a canonical UUIDv7")))
		return
	}
	worker := workerFromContext(r.Context())
	var row db.RuntimeSubstrate
	err = s.inTx(r.Context(), func(work *txWork) error {
		authority, err := work.q.LockRuntimeSubstrateAuthority(r.Context(), db.LockRuntimeSubstrateAuthorityParams{
			DeploymentDefinitionID: pgvalue.UUID(workspaceDefinitionID),
			WorkerInstanceID:       pgvalue.UUID(worker.WorkerInstanceID),
			WorkerGroupID:          worker.WorkerGroupID,
			WorkerEpoch:            worker.WorkerEpoch,
			SubstrateFormat:        strings.TrimSpace(request.Format),
			BuilderAbi:             strings.TrimSpace(request.BuilderABI),
			LayoutAbi:              strings.TrimSpace(request.LayoutABI),
		})
		if isNoRows(err) {
			return conflict(errors.New("runtime substrate authority is stale"))
		}
		if err != nil {
			return errors.New("lock runtime substrate authority")
		}
		params := db.InsertRuntimeSubstrateParams{
			ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                  authority.OrgID,
			ProjectID:              authority.ProjectID,
			EnvironmentID:          authority.EnvironmentID,
			DeploymentDefinitionID: authority.DeploymentDefinitionID,
			SubstrateDigest:        strings.TrimSpace(request.SubstrateDigest),
			SubstrateFormat:        strings.TrimSpace(request.Format),
			BuilderAbi:             strings.TrimSpace(request.BuilderABI),
			LayoutAbi:              strings.TrimSpace(request.LayoutABI),
			SubstrateSizeBytes:     request.SizeBytes,
		}
		if _, err = work.q.InsertRuntimeSubstrate(r.Context(), params); err != nil {
			return err
		}
		row, err = work.q.GetRuntimeSubstrateRegistration(r.Context(), db.GetRuntimeSubstrateRegistrationParams{
			OrgID:                  params.OrgID,
			ProjectID:              params.ProjectID,
			EnvironmentID:          params.EnvironmentID,
			DeploymentDefinitionID: params.DeploymentDefinitionID,
			SubstrateDigest:        params.SubstrateDigest,
			SubstrateFormat:        params.SubstrateFormat,
			BuilderAbi:             params.BuilderAbi,
			LayoutAbi:              params.LayoutAbi,
			SubstrateSizeBytes:     params.SubstrateSizeBytes,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict(errors.New("runtime substrate output conflicts with the registered build"))
		}
		return err
	})
	if err != nil {
		if errorStatus(err) == http.StatusInternalServerError {
			writeError(w, errors.New("register runtime substrate"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RuntimeSubstrateRegisterResponse{
		RuntimeSubstrate: runtimeSubstrateResponse(row),
	})
}

func validateRuntimeSubstrateRegisterRequest(request workerapi.RuntimeSubstrateRegisterRequest) error {
	required := map[string]string{
		"deployment_definition_id": request.DeploymentDefinitionID,
		"substrate_digest":         request.SubstrateDigest,
		"format":                   request.Format,
		"builder_abi":              request.BuilderABI,
		"layout_abi":               request.LayoutABI,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if !taskWorkspaceDigestPattern.MatchString(request.SubstrateDigest) {
		return errors.New("substrate_digest must be a SHA-256 digest")
	}
	if request.SizeBytes < 0 {
		return errors.New("size_bytes must be non-negative")
	}
	return nil
}

func runtimeSubstrateResponse(row db.RuntimeSubstrate) workerapi.RuntimeSubstrate {
	return workerapi.RuntimeSubstrate{
		ID:                     pgvalue.UUIDString(row.ID),
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		SubstrateDigest:        row.SubstrateDigest,
		Format:                 row.SubstrateFormat,
		BuilderABI:             row.BuilderAbi,
		LayoutABI:              row.LayoutAbi,
		SizeBytes:              row.SubstrateSizeBytes,
	}
}
