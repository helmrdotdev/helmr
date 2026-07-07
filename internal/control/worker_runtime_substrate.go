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

func (s *Server) workerRegisterRuntimeSubstrateArtifact(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeSubstrateArtifactRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime substrate artifact register request JSON: %w", err)))
		return
	}
	if err := validateRuntimeSubstrateArtifactRegisterRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if s.cas == nil {
		writeError(w, errors.New("runtime substrate CAS is not configured"))
		return
	}
	deploymentSandboxID, err := parseWorkspaceUUID("deployment_sandbox_id", request.DeploymentSandboxID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runtimeSubstrateArtifactID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if strings.TrimSpace(request.ID) != "" {
		runtimeSubstrateArtifactID, err = parseWorkspaceUUID("id", request.ID)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.GetDeploymentSandboxForWorkerGroup(r.Context(), db.GetDeploymentSandboxForWorkerGroupParams{
		ID:            deploymentSandboxID,
		WorkerGroupID: worker.WorkerGroupID,
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("deployment sandbox not found")))
		return
	} else if err != nil {
		writeError(w, errors.New("load deployment sandbox"))
		return
	}
	stat, err := s.cas.Stat(r.Context(), strings.TrimSpace(request.Artifact.Digest))
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("runtime substrate artifact is missing from CAS: %w", err)))
		return
	}
	if stat.SizeBytes != request.Artifact.SizeBytes {
		writeError(w, badRequest(errors.New("runtime substrate artifact size_bytes mismatch")))
		return
	}
	if strings.TrimSpace(stat.MediaType) != strings.TrimSpace(request.Artifact.MediaType) {
		writeError(w, badRequest(errors.New("runtime substrate artifact media_type mismatch")))
		return
	}
	if strings.TrimSpace(request.Artifact.MediaType) != cas.RuntimeSubstrateMediaType {
		writeError(w, badRequest(fmt.Errorf("runtime substrate artifact media_type must be %s", cas.RuntimeSubstrateMediaType)))
		return
	}
	var row db.RuntimeSubstrate
	err = s.inTx(r.Context(), func(work *txWork) error {
		sandbox, err := work.q.GetDeploymentSandboxForWorkerGroup(r.Context(), db.GetDeploymentSandboxForWorkerGroupParams{
			ID:            deploymentSandboxID,
			WorkerGroupID: worker.WorkerGroupID,
		})
		if isNoRows(err) {
			return notFound(errors.New("deployment sandbox not found"))
		}
		if err != nil {
			return errors.New("load deployment sandbox")
		}
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID:     sandbox.OrgID,
			Digest:    strings.TrimSpace(request.Artifact.Digest),
			SizeBytes: request.Artifact.SizeBytes,
			MediaType: strings.TrimSpace(request.Artifact.MediaType),
		}); err != nil {
			return errors.New("record runtime substrate CAS object")
		}
		artifact, err := work.q.UpsertRuntimeSubstrateArtifactBlob(r.Context(), db.UpsertRuntimeSubstrateArtifactBlobParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     sandbox.OrgID,
			ProjectID:                 sandbox.ProjectID,
			EnvironmentID:             sandbox.EnvironmentID,
			Digest:                    strings.TrimSpace(request.Artifact.Digest),
			SizeBytes:                 request.Artifact.SizeBytes,
			MediaType:                 strings.TrimSpace(request.Artifact.MediaType),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return errors.New("record runtime substrate artifact")
		}
		row, err = work.q.UpsertRuntimeSubstrateArtifact(r.Context(), db.UpsertRuntimeSubstrateArtifactParams{
			ID:                        runtimeSubstrateArtifactID,
			OrgID:                     sandbox.OrgID,
			WorkerGroupID:             worker.WorkerGroupID,
			ProjectID:                 sandbox.ProjectID,
			EnvironmentID:             sandbox.EnvironmentID,
			DeploymentSandboxID:       sandbox.ID,
			ArtifactID:                artifact.ID,
			SubstrateDigest:           strings.TrimSpace(request.SubstrateDigest),
			SubstrateFormat:           strings.TrimSpace(request.Format),
			BuilderAbi:                strings.TrimSpace(request.BuilderABI),
			LayoutAbi:                 strings.TrimSpace(request.LayoutABI),
			SubstrateSizeBytes:        request.SizeBytes,
			Source:                    normalizedJSONRawMessage(request.Source),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		return err
	})
	if err != nil {
		if errorStatus(err) == http.StatusInternalServerError {
			writeError(w, errors.New("upsert runtime substrate artifact"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRuntimeSubstrateArtifactRegisterResponse{
		RuntimeSubstrateArtifact: runtimeSubstrateArtifactResponse(row, request.Artifact),
	})
}

func (s *Server) workerLookupRuntimeSubstrateArtifact(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeSubstrateArtifactLookupRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime substrate artifact lookup request JSON: %w", err)))
		return
	}
	if err := validateRuntimeSubstrateArtifactLookupRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	deploymentSandboxID, err := parseWorkspaceUUID("deployment_sandbox_id", request.DeploymentSandboxID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	sandbox, err := s.db.GetDeploymentSandboxForWorkerGroup(r.Context(), db.GetDeploymentSandboxForWorkerGroupParams{
		ID:            deploymentSandboxID,
		WorkerGroupID: worker.WorkerGroupID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("deployment sandbox not found")))
		return
	}
	if err != nil {
		s.log.Error("lookup runtime substrate sandbox failed", "worker_instance_id", worker.WorkerInstanceID.String(), "deployment_sandbox_id", request.DeploymentSandboxID, "error", err)
		writeError(w, errors.New("lookup runtime substrate sandbox"))
		return
	}
	row, err := s.db.GetRuntimeSubstrateArtifactForSandbox(r.Context(), db.GetRuntimeSubstrateArtifactForSandboxParams{
		OrgID:               sandbox.OrgID,
		WorkerGroupID:       worker.WorkerGroupID,
		ProjectID:           sandbox.ProjectID,
		EnvironmentID:       sandbox.EnvironmentID,
		DeploymentSandboxID: sandbox.ID,
		SubstrateDigest:     strings.TrimSpace(request.SubstrateDigest),
		SubstrateFormat:     strings.TrimSpace(request.Format),
		BuilderAbi:          strings.TrimSpace(request.BuilderABI),
		LayoutAbi:           strings.TrimSpace(request.LayoutABI),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("runtime substrate artifact not found")))
		return
	}
	if err != nil {
		s.log.Error("lookup runtime substrate artifact failed", "worker_instance_id", worker.WorkerInstanceID.String(), "deployment_sandbox_id", request.DeploymentSandboxID, "error", err)
		writeError(w, errors.New("lookup runtime substrate artifact"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRuntimeSubstrateArtifactLookupResponse{
		RuntimeSubstrateArtifact: runtimeSubstrateArtifactResponseFromLookup(row),
	})
}

func validateRuntimeSubstrateArtifactRegisterRequest(request api.WorkerRuntimeSubstrateArtifactRegisterRequest) error {
	required := map[string]string{
		"deployment_sandbox_id": request.DeploymentSandboxID,
		"artifact.digest":       request.Artifact.Digest,
		"artifact.media_type":   request.Artifact.MediaType,
		"substrate_digest":      request.SubstrateDigest,
		"format":                request.Format,
		"builder_abi":           request.BuilderABI,
		"layout_abi":            request.LayoutABI,
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

func validateRuntimeSubstrateArtifactLookupRequest(request api.WorkerRuntimeSubstrateArtifactLookupRequest) error {
	required := map[string]string{
		"deployment_sandbox_id": request.DeploymentSandboxID,
		"substrate_digest":      request.SubstrateDigest,
		"format":                request.Format,
		"builder_abi":           request.BuilderABI,
		"layout_abi":            request.LayoutABI,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	return nil
}

func runtimeSubstrateArtifactResponse(row db.RuntimeSubstrate, artifact api.CASObject) api.WorkerRuntimeSubstrateArtifact {
	return api.WorkerRuntimeSubstrateArtifact{
		ID:                  pgvalue.UUIDString(row.ID),
		DeploymentSandboxID: pgvalue.UUIDString(row.DeploymentSandboxID),
		Artifact:            artifact,
		SubstrateDigest:     row.SubstrateDigest,
		Format:              row.SubstrateFormat,
		BuilderABI:          row.BuilderAbi,
		LayoutABI:           row.LayoutAbi,
		SizeBytes:           row.SubstrateSizeBytes,
	}
}

func runtimeSubstrateArtifactResponseFromLookup(row db.GetRuntimeSubstrateArtifactForSandboxRow) api.WorkerRuntimeSubstrateArtifact {
	return api.WorkerRuntimeSubstrateArtifact{
		ID:                  pgvalue.UUIDString(row.ID),
		DeploymentSandboxID: pgvalue.UUIDString(row.DeploymentSandboxID),
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
