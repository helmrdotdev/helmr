package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRegisterRuntimeSubstrate(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeSubstrateRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime substrate register request JSON: %w", err)))
		return
	}
	if err := validateRuntimeSubstrateRegisterRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if s.cas == nil {
		writeError(w, errors.New("runtime substrate CAS is not configured"))
		return
	}
	workspaceDefinitionID, err := parseWorkspaceUUID("deployment_definition_id", request.DeploymentDefinitionID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runtimeSubstrateID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if strings.TrimSpace(request.ID) != "" {
		runtimeSubstrateID, err = parseWorkspaceUUID("id", request.ID)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	worker := workerFromContext(r.Context())
	stat, err := s.cas.Stat(r.Context(), strings.TrimSpace(request.Artifact.Digest))
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("runtime substrate is missing from CAS: %w", err)))
		return
	}
	if stat.SizeBytes != request.Artifact.SizeBytes {
		writeError(w, badRequest(errors.New("runtime substrate size_bytes mismatch")))
		return
	}
	if strings.TrimSpace(stat.MediaType) != strings.TrimSpace(request.Artifact.MediaType) {
		writeError(w, badRequest(errors.New("runtime substrate media_type mismatch")))
		return
	}
	if strings.TrimSpace(request.Artifact.MediaType) != cas.RuntimeSubstrateMediaType {
		writeError(w, badRequest(fmt.Errorf("runtime substrate media_type must be %s", cas.RuntimeSubstrateMediaType)))
		return
	}
	var row db.RuntimeSubstrate
	err = s.inTx(r.Context(), func(work *txWork) error {
		authority, err := work.q.LockRuntimeSubstrateAuthority(r.Context(), db.LockRuntimeSubstrateAuthorityParams{
			DeploymentDefinitionID: workspaceDefinitionID,
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
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID:     authority.OrgID,
			Digest:    strings.TrimSpace(request.Artifact.Digest),
			SizeBytes: request.Artifact.SizeBytes,
			MediaType: strings.TrimSpace(request.Artifact.MediaType),
		}); err != nil {
			return errors.New("record runtime substrate CAS object")
		}
		artifact, err := work.q.UpsertRuntimeSubstrateBlob(r.Context(), db.UpsertRuntimeSubstrateBlobParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     authority.OrgID,
			ProjectID:                 authority.ProjectID,
			EnvironmentID:             authority.EnvironmentID,
			Digest:                    strings.TrimSpace(request.Artifact.Digest),
			SizeBytes:                 request.Artifact.SizeBytes,
			MediaType:                 strings.TrimSpace(request.Artifact.MediaType),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return errors.New("record runtime substrate")
		}
		params := db.InsertRuntimeSubstrateParams{
			ID:                        runtimeSubstrateID,
			OrgID:                     authority.OrgID,
			ProjectID:                 authority.ProjectID,
			EnvironmentID:             authority.EnvironmentID,
			DeploymentDefinitionID:    authority.DeploymentDefinitionID,
			ArtifactID:                artifact.ID,
			SubstrateDigest:           strings.TrimSpace(request.SubstrateDigest),
			SubstrateFormat:           strings.TrimSpace(request.Format),
			BuilderAbi:                strings.TrimSpace(request.BuilderABI),
			LayoutAbi:                 strings.TrimSpace(request.LayoutABI),
			SubstrateSizeBytes:        request.SizeBytes,
			Source:                    normalizedJSONRawMessage(request.Source),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		}
		if _, err = work.q.InsertRuntimeSubstrate(r.Context(), params); err != nil {
			return err
		}
		row, err = work.q.GetRuntimeSubstrateRegistration(r.Context(), db.GetRuntimeSubstrateRegistrationParams{
			OrgID:                  params.OrgID,
			ProjectID:              params.ProjectID,
			EnvironmentID:          params.EnvironmentID,
			DeploymentDefinitionID: params.DeploymentDefinitionID,
			ArtifactID:             params.ArtifactID,
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
	writeJSON(w, http.StatusOK, api.WorkerRuntimeSubstrateRegisterResponse{
		RuntimeSubstrate: runtimeSubstrateResponse(row, request.Artifact),
	})
}

func (s *Server) workerLookupRuntimeSubstrate(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeSubstrateLookupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime substrate lookup request JSON: %w", err)))
		return
	}
	if err := validateRuntimeSubstrateLookupRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	workspaceDefinitionID, err := parseWorkspaceUUID("deployment_definition_id", request.DeploymentDefinitionID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	var row db.GetRuntimeSubstrateForWorkspaceDefinitionRow
	err = s.inTx(r.Context(), func(work *txWork) error {
		authority, err := work.q.LockRuntimeSubstrateAuthority(r.Context(), db.LockRuntimeSubstrateAuthorityParams{
			DeploymentDefinitionID: workspaceDefinitionID,
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
		row, err = work.q.GetRuntimeSubstrateForWorkspaceDefinition(r.Context(), db.GetRuntimeSubstrateForWorkspaceDefinitionParams{
			OrgID:                  authority.OrgID,
			ProjectID:              authority.ProjectID,
			EnvironmentID:          authority.EnvironmentID,
			DeploymentDefinitionID: authority.DeploymentDefinitionID,
			SubstrateDigest:        strings.TrimSpace(request.SubstrateDigest),
			SubstrateFormat:        strings.TrimSpace(request.Format),
			BuilderAbi:             strings.TrimSpace(request.BuilderABI),
			LayoutAbi:              strings.TrimSpace(request.LayoutABI),
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("runtime substrate not found")))
		return
	}
	if err != nil {
		if errorStatus(err) != http.StatusInternalServerError {
			writeError(w, err)
			return
		}
		s.log.Error("lookup runtime substrate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "deployment_definition_id", request.DeploymentDefinitionID, "error", err)
		writeError(w, errors.New("lookup runtime substrate"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRuntimeSubstrateLookupResponse{
		RuntimeSubstrate: runtimeSubstrateResponseFromLookup(row),
	})
}

func validateRuntimeSubstrateRegisterRequest(request api.WorkerRuntimeSubstrateRegisterRequest) error {
	required := map[string]string{
		"deployment_definition_id": request.DeploymentDefinitionID,
		"artifact.digest":          request.Artifact.Digest,
		"artifact.media_type":      request.Artifact.MediaType,
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
	if request.Artifact.SizeBytes < 0 {
		return errors.New("artifact.size_bytes must be non-negative")
	}
	if request.SizeBytes < 0 {
		return errors.New("size_bytes must be non-negative")
	}
	if len(request.Source) > 0 && !json.Valid(request.Source) {
		return errors.New("source must be valid JSON")
	}
	return nil
}

func validateRuntimeSubstrateLookupRequest(request api.WorkerRuntimeSubstrateLookupRequest) error {
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
	return nil
}

func runtimeSubstrateResponse(row db.RuntimeSubstrate, artifact api.CASObject) api.WorkerRuntimeSubstrate {
	return api.WorkerRuntimeSubstrate{
		ID:                     pgvalue.UUIDString(row.ID),
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		Artifact:               artifact,
		SubstrateDigest:        row.SubstrateDigest,
		Format:                 row.SubstrateFormat,
		BuilderABI:             row.BuilderAbi,
		LayoutABI:              row.LayoutAbi,
		SizeBytes:              row.SubstrateSizeBytes,
		Retired:                row.RetiredAt.Valid,
	}
}

func runtimeSubstrateResponseFromLookup(row db.GetRuntimeSubstrateForWorkspaceDefinitionRow) api.WorkerRuntimeSubstrate {
	return api.WorkerRuntimeSubstrate{
		ID:                     pgvalue.UUIDString(row.ID),
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		Artifact: api.CASObject{
			Digest:    row.ArtifactDigest,
			SizeBytes: row.ArtifactSizeBytes,
			MediaType: row.ArtifactMediaType,
		},
		SubstrateDigest: row.SubstrateDigest,
		Format:          row.SubstrateFormat,
		BuilderABI:      row.BuilderAbi,
		LayoutABI:       row.LayoutAbi,
		SizeBytes:       row.SubstrateSizeBytes,
	}
}
