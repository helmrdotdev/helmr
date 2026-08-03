package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerNextPlatformAcquisition(w http.ResponseWriter, r *http.Request) {
	if s.buildPolicy == nil || s.platformStore == nil {
		writeError(w, unavailable(errors.New("Platform Artifact authority is not configured")))
		return
	}
	var request workerapi.PlatformAcquisitionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Platform acquisition JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.GetNextDeploymentPlatformAcquisition(
		r.Context(),
		db.GetNextDeploymentPlatformAcquisitionParams{
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerEpoch:           pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
			WorkerProtocolVersion: worker.ProtocolVersion,
		},
	)
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, workerapi.PlatformAcquisitionResponse{})
		return
	}
	if err != nil {
		writeError(w, errors.New("read next Platform acquisition"))
		return
	}
	policyDigest, err := s.buildPolicy.Digest()
	if err != nil {
		writeError(w, unavailable(err))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PlatformAcquisitionResponse{
		Acquisition: platformAcquisitionFromRow(row, policyDigest),
	})
}

func (s *Server) workerCompletePlatformAcquisition(w http.ResponseWriter, r *http.Request) {
	if s.buildPolicy == nil || s.platformStore == nil || s.platformArtifactLocks == nil {
		writeError(w, unavailable(errors.New("Platform Artifact authority is not configured")))
		return
	}
	var request workerapi.PlatformAcquisitionCompleteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Platform acquisition completion JSON: %w", err)))
		return
	}
	row, err := s.platformAcquisitionRow(r.Context(), request.Acquisition)
	if err != nil {
		writeError(w, err)
		return
	}
	candidates := request.Candidates
	if err := validatePlatformCandidateEnvelope(candidates); err != nil {
		writeError(w, badRequest(err))
		return
	}
	digests := []string{
		candidates.Runtime.Digest,
		candidates.Manager.Digest,
		candidates.Toolchain.Digest,
	}
	err = s.platformArtifactLocks.With(r.Context(), digests, func() error {
		if err := s.validatePlatformCandidateObjects(r.Context(), request); err != nil {
			return err
		}
		runtimeDigest, _ := deployment.SHA256DigestBytes(candidates.Runtime.Digest)
		managerDigest, _ := deployment.SHA256DigestBytes(candidates.Manager.Digest)
		toolchainDigest, _ := deployment.SHA256DigestBytes(candidates.Toolchain.Digest)
		_, err := s.db.PinDeploymentPlatformArtifacts(
			r.Context(),
			db.PinDeploymentPlatformArtifactsParams{
				OrgID:                row.OrgID,
				ProjectID:            row.ProjectID,
				EnvironmentID:        row.EnvironmentID,
				ID:                   row.ID,
				BuildRuntimeDigest:   runtimeDigest,
				BuildToolchainDigest: toolchainDigest,
				BuildManagerDigest:   managerDigest,
			},
		)
		if isNoRows(err) {
			return conflict(errors.New("Deployment Platform Artifact pins changed"))
		}
		if err != nil {
			return fmt.Errorf("pin Deployment Platform Artifacts: %w", err)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PlatformAcquisitionResult{
		DeploymentID: request.Acquisition.DeploymentID,
		Status:       "pinned",
	})
}

func (s *Server) workerFailPlatformAcquisition(w http.ResponseWriter, r *http.Request) {
	if s.buildPolicy == nil {
		writeError(w, unavailable(errors.New("Platform Artifact authority is not configured")))
		return
	}
	var request workerapi.PlatformAcquisitionFailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Platform acquisition failure JSON: %w", err)))
		return
	}
	if !validPlatformAcquisitionFailureReason(request.Reason) {
		writeError(w, badRequest(errors.New("Platform acquisition failure reason is invalid")))
		return
	}
	var detail map[string]any
	if len(request.Error) == 0 || json.Unmarshal(request.Error, &detail) != nil || detail == nil {
		writeError(w, badRequest(errors.New("Platform acquisition failure error must be an object")))
		return
	}
	row, err := s.platformAcquisitionRow(r.Context(), request.Acquisition)
	if err != nil {
		writeError(w, err)
		return
	}
	failure, err := json.Marshal(struct {
		Reason workerapi.PlatformAcquisitionFailureReason `json:"reason_code"`
		Error  json.RawMessage                            `json:"error"`
	}{
		Reason: request.Reason,
		Error:  request.Error,
	})
	if err != nil {
		writeError(w, errors.New("encode Platform acquisition failure"))
		return
	}
	if _, err := s.db.FailDeploymentPlatformAcquisition(
		r.Context(),
		db.FailDeploymentPlatformAcquisitionParams{
			Failure:       failure,
			ID:            row.ID,
			OrgID:         row.OrgID,
			ProjectID:     row.ProjectID,
			EnvironmentID: row.EnvironmentID,
		},
	); isNoRows(err) {
		writeError(w, conflict(errors.New("Deployment Platform acquisition changed")))
		return
	} else if err != nil {
		writeError(w, errors.New("fail Deployment Platform acquisition"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PlatformAcquisitionResult{
		DeploymentID: request.Acquisition.DeploymentID,
		Status:       "failed",
	})
}

func (s *Server) platformAcquisitionRow(
	ctx context.Context,
	input workerapi.PlatformAcquisition,
) (db.GetDeploymentPlatformAcquisitionRow, error) {
	worker := workerFromContext(ctx)
	deploymentID, err := ids.Parse(input.DeploymentID)
	if err != nil {
		return db.GetDeploymentPlatformAcquisitionRow{}, badRequest(errors.New("Platform acquisition Deployment ID is invalid"))
	}
	row, err := s.db.GetDeploymentPlatformAcquisition(
		ctx,
		db.GetDeploymentPlatformAcquisitionParams{
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerEpoch:           pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
			WorkerProtocolVersion: worker.ProtocolVersion,
			ID:                    pgvalue.UUID(deploymentID),
		},
	)
	if isNoRows(err) {
		return db.GetDeploymentPlatformAcquisitionRow{}, conflict(errors.New("Platform acquisition is stale"))
	}
	if err != nil {
		return db.GetDeploymentPlatformAcquisitionRow{}, errors.New("read Platform acquisition")
	}
	policyDigest, err := s.buildPolicy.Digest()
	if err != nil {
		return db.GetDeploymentPlatformAcquisitionRow{}, unavailable(err)
	}
	expected := platformAcquisitionFromRow(row, policyDigest)
	if input != *expected {
		return db.GetDeploymentPlatformAcquisitionRow{}, conflict(errors.New("Platform acquisition does not exact-match Control authority"))
	}
	return row, nil
}

type platformAcquisitionRow interface {
	db.GetNextDeploymentPlatformAcquisitionRow | db.GetDeploymentPlatformAcquisitionRow
}

func platformAcquisitionFromRow[T platformAcquisitionRow](
	row T,
	policyDigest string,
) *workerapi.PlatformAcquisition {
	switch value := any(row).(type) {
	case db.GetNextDeploymentPlatformAcquisitionRow:
		return newPlatformAcquisition(
			value.ID,
			value.OrgID,
			value.ProjectID,
			value.EnvironmentID,
			value.BuildNodeVersion,
			value.BuildManagerName,
			value.BuildManagerVersion,
			value.BuildManagerIntegrity,
			value.BuildContractVersion,
			policyDigest,
		)
	case db.GetDeploymentPlatformAcquisitionRow:
		return newPlatformAcquisition(
			value.ID,
			value.OrgID,
			value.ProjectID,
			value.EnvironmentID,
			value.BuildNodeVersion,
			value.BuildManagerName,
			value.BuildManagerVersion,
			value.BuildManagerIntegrity,
			value.BuildContractVersion,
			policyDigest,
		)
	default:
		panic("unsupported Platform acquisition row")
	}
}

func newPlatformAcquisition(
	id,
	orgID,
	projectID,
	environmentID pgtype.UUID,
	nodeVersion,
	managerName,
	managerVersion string,
	managerIntegrity pgtype.Text,
	buildContract,
	policyDigest string,
) *workerapi.PlatformAcquisition {
	return &workerapi.PlatformAcquisition{
		DeploymentID:      pgvalue.MustUUIDValue(id).String(),
		OrgID:             pgvalue.MustUUIDValue(orgID).String(),
		ProjectID:         pgvalue.MustUUIDValue(projectID).String(),
		EnvironmentID:     pgvalue.MustUUIDValue(environmentID).String(),
		NodeVersion:       nodeVersion,
		ManagerName:       managerName,
		ManagerVersion:    managerVersion,
		ManagerIntegrity:  managerIntegrity.String,
		BuildContract:     buildContract,
		BuildPolicyDigest: policyDigest,
	}
}

func validatePlatformCandidateEnvelope(candidates workerapi.PlatformAcquisitionCandidates) error {
	values := []struct {
		name      string
		candidate workerapi.CASObject
		mediaType string
		maxBytes  int64
	}{
		{"Runtime", candidates.Runtime, deployment.RuntimeArtifactMediaType, 3 << 30},
		{"Manager", candidates.Manager, deployment.ManagerTreeMediaType, 512 << 20},
		{"toolchain", candidates.Toolchain, deployment.ToolchainMediaType, 4 << 30},
	}
	for _, value := range values {
		if _, err := deployment.SHA256DigestBytes(value.candidate.Digest); err != nil {
			return fmt.Errorf("%s candidate digest is invalid", value.name)
		}
		if value.candidate.SizeBytes < 1 || value.candidate.SizeBytes > value.maxBytes {
			return fmt.Errorf("%s candidate size is invalid", value.name)
		}
		if value.candidate.MediaType != value.mediaType {
			return fmt.Errorf("%s candidate media type is invalid", value.name)
		}
	}
	return nil
}

func (s *Server) validatePlatformCandidateObjects(
	ctx context.Context,
	request workerapi.PlatformAcquisitionCompleteRequest,
) (returnErr error) {
	manager := deployment.PackageManager{
		Integrity: request.Acquisition.ManagerIntegrity,
		Name:      deployment.PackageManagerName(request.Acquisition.ManagerName),
		Version:   request.Acquisition.ManagerVersion,
	}
	expectations, err := s.buildPolicy.PlatformArtifactExpectations(
		request.Acquisition.NodeVersion,
		manager,
		request.Candidates.Runtime.Digest,
	)
	if err != nil {
		return conflict(err)
	}
	type candidateFile struct {
		object workerapi.CASObject
		file   *os.File
	}
	files := make([]candidateFile, 0, 3)
	defer func() {
		for _, value := range files {
			if value.file != nil {
				returnErr = errors.Join(returnErr, value.file.Close())
			}
		}
	}()
	for _, candidate := range []workerapi.CASObject{
		request.Candidates.Runtime,
		request.Candidates.Manager,
		request.Candidates.Toolchain,
	} {
		if s.buildPolicy.DeniesDigest(candidate.Digest) {
			return conflict(errors.New("Platform Artifact digest is denied"))
		}
		object, err := s.platformStore.Stat(ctx, candidate.Digest)
		if err != nil {
			return fmt.Errorf("stat Platform Artifact %s: %w", candidate.Digest, err)
		}
		if object.Digest != candidate.Digest ||
			object.SizeBytes != candidate.SizeBytes ||
			object.MediaType != candidate.MediaType {
			return conflict(errors.New("Platform Artifact metadata does not exact-match the candidate"))
		}
		body, err := s.platformStore.Get(ctx, candidate.Digest)
		if err != nil {
			return fmt.Errorf("open Platform Artifact %s: %w", candidate.Digest, err)
		}
		file, err := os.CreateTemp("", ".helmr-platform-artifact-*")
		if err != nil {
			_ = body.Close()
			return fmt.Errorf("create Platform Artifact snapshot: %w", err)
		}
		if err := os.Remove(file.Name()); err != nil {
			_ = body.Close()
			_ = file.Close()
			return fmt.Errorf("unlink Platform Artifact snapshot: %w", err)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(
			io.MultiWriter(hash, file),
			io.LimitReader(body, candidate.SizeBytes+1),
		)
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil {
			_ = file.Close()
			return errors.Join(copyErr, closeErr)
		}
		actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if size != candidate.SizeBytes || actual != candidate.Digest {
			_ = file.Close()
			return conflict(errors.New("Platform Artifact bytes do not exact-match the candidate"))
		}
		files = append(files, candidateFile{object: candidate, file: file})
	}
	runtime, err := deployment.InspectPlatformArtifact(
		ctx,
		files[0].file,
		platformDescriptor(files[0].object),
		expectations.Runtime,
	)
	if err != nil {
		return conflict(fmt.Errorf("validate Runtime candidate: %w", err))
	}
	if _, err := deployment.InspectPlatformArtifact(
		ctx,
		files[1].file,
		platformDescriptor(files[1].object),
		expectations.Manager,
	); err != nil {
		return conflict(fmt.Errorf("validate Manager candidate: %w", err))
	}
	toolchain, err := deployment.InspectPlatformArtifact(
		ctx,
		files[2].file,
		platformDescriptor(files[2].object),
		expectations.Toolchain,
	)
	if err != nil {
		return conflict(fmt.Errorf("validate toolchain candidate: %w", err))
	}
	if err := validatePlatformCandidateBinding(
		runtime,
		toolchain,
		files[0].object.Digest,
	); err != nil {
		return conflict(err)
	}
	return nil
}

func validatePlatformCandidateBinding(
	runtime deployment.InspectedPlatformArtifact,
	toolchain deployment.InspectedPlatformArtifact,
	runtimeDigest string,
) error {
	if runtime.Runtime == nil ||
		toolchain.Toolchain == nil ||
		toolchain.Toolchain.RuntimeDigest != runtimeDigest ||
		toolchain.Toolchain.NodeVersion != runtime.Runtime.NodeVersion ||
		toolchain.Toolchain.NodeModuleABI != runtime.Runtime.NodeModuleABI ||
		toolchain.Toolchain.NodeSource != runtime.Runtime.Source {
		return errors.New(
			"Runtime and toolchain candidates do not share one Node authority",
		)
	}
	return nil
}

func platformDescriptor(value workerapi.CASObject) deployment.ArtifactDescriptor {
	return deployment.ArtifactDescriptor{
		Digest:    value.Digest,
		MediaType: value.MediaType,
		SizeBytes: value.SizeBytes,
	}
}

func validPlatformAcquisitionFailureReason(reason workerapi.PlatformAcquisitionFailureReason) bool {
	switch reason {
	case workerapi.PlatformAcquisitionUnsupportedSelector,
		workerapi.PlatformAcquisitionOriginRejected,
		workerapi.PlatformAcquisitionIntegrityFailed,
		workerapi.PlatformAcquisitionTopologyFailed,
		workerapi.PlatformAcquisitionConformanceFailed,
		workerapi.PlatformAcquisitionDenied:
		return true
	default:
		return false
	}
}
