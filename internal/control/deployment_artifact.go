package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

type deploymentVersionMetadata struct {
	APIVersion            string
	SDKVersion            string
	CLIVersion            string
	BundleFormatVersion   int32
	WorkerProtocolVersion string
}

type casObjectLookupStore interface {
	GetCasObject(context.Context, string) (db.CasObject, error)
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	if s.cas == nil {
		writeError(w, unavailable(errors.New("deployment source artifact storage is not configured")))
		return
	}
	reader, request, err := s.receiveDeploymentMetadata(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, err := deploymentMetadataFromRequest(r, request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	archivePath, cleanup, err := receiveDeploymentArchive(reader)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	defer cleanup()
	if err := validateDeploymentSourceArtifactArchive(archivePath); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment source artifact: %w", err)))
		return
	}
	if err := validateDeploymentContentHash(archivePath, request.ContentHash); err != nil {
		writeError(w, badRequest(err))
		return
	}
	file, err := os.Open(archivePath)
	if err != nil {
		writeError(w, errors.New("open deployment source artifact"))
		return
	}
	artifactObject, err := s.cas.Put(r.Context(), api.DeploymentSourceArtifactMediaType, file)
	closeErr := file.Close()
	if err != nil {
		writeError(w, fmt.Errorf("store deployment source artifact: %w", err))
		return
	}
	if closeErr != nil {
		writeError(w, fmt.Errorf("close deployment source artifact: %w", closeErr))
		return
	}
	artifact := api.DeploymentSourceArtifact{
		Digest:    artifactObject.Digest,
		SizeBytes: artifactObject.SizeBytes,
		MediaType: artifactObject.MediaType,
	}
	cleanupArtifact := func() {
		s.deleteUnreferencedDeploymentSourceArtifact(r.Context(), artifact.Digest)
	}
	var response api.DeploymentResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		store, ok := work.q.(deploymentStore)
		if !ok {
			return unavailable(errors.New("deployment storage is not configured"))
		}
		var createErr error
		response, createErr = createDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, strings.TrimSpace(request.ContentHash), artifact, metadata)
		return createErr
	})
	if err != nil {
		cleanupArtifact()
		writeDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func deploymentMetadataFromRequest(r *http.Request, request api.CreateDeploymentRequest) (deploymentVersionMetadata, error) {
	apiVersion := firstNonEmptyString(request.APIVersion, requestAPIVersion(r))
	if apiVersion != api.CurrentAPIVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported deployment api_version %q; current version is %s", apiVersion, api.CurrentAPIVersion)
	}
	bundleFormatVersion := request.BundleFormatVersion
	if bundleFormatVersion == 0 {
		bundleFormatVersion = api.CurrentBundleFormatVersion
	}
	if bundleFormatVersion != api.CurrentBundleFormatVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported bundle_format_version %d; current version is %d", bundleFormatVersion, api.CurrentBundleFormatVersion)
	}
	workerProtocolVersion := firstNonEmptyString(request.WorkerProtocolVersion, api.CurrentWorkerProtocolVersion)
	if workerProtocolVersion != api.CurrentWorkerProtocolVersion {
		return deploymentVersionMetadata{}, fmt.Errorf("unsupported worker_protocol_version %q; current version is %s", workerProtocolVersion, api.CurrentWorkerProtocolVersion)
	}
	return deploymentVersionMetadata{
		APIVersion:            apiVersion,
		SDKVersion:            firstNonEmptyString(request.SDKVersion, r.Header.Get(api.SDKVersionHeader)),
		CLIVersion:            firstNonEmptyString(request.CLIVersion, r.Header.Get(api.CLIVersionHeader), r.Header.Get(api.ClientVersionHeader)),
		BundleFormatVersion:   bundleFormatVersion,
		WorkerProtocolVersion: workerProtocolVersion,
	}, nil
}

func (s *Server) receiveDeploymentMetadata(r *http.Request) (*multipart.Reader, api.CreateDeploymentRequest, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment multipart form: %w", err)
	}
	var request api.CreateDeploymentRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata is required")
		}
		if err != nil {
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		name := part.FormName()
		switch name {
		case "metadata":
			metadata, err := readLimitedFormField(part, 1<<20)
			if err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment metadata: %w", err)
			}
			decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(metadata)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment metadata JSON: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must contain a single JSON value")
			}
			return reader, request, nil
		case "deployment_source":
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must precede deployment_source")
		default:
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("unexpected deployment multipart field %q", name)
		}
	}
}

func receiveDeploymentArchive(reader *multipart.Reader) (string, func(), error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return "", func() {}, errors.New("deployment_source file is required")
		}
		if err != nil {
			return "", func() {}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		if part.FormName() != "deployment_source" {
			part.Close()
			continue
		}
		defer part.Close()
		tmp, err := os.CreateTemp("", "helmr-deployment-source-*.tar")
		if err != nil {
			return "", func() {}, fmt.Errorf("create deployment source temp file: %w", err)
		}
		path := tmp.Name()
		cleanup := func() { _ = os.Remove(path) }
		if _, err := io.Copy(tmp, part); err != nil {
			_ = tmp.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("copy deployment source artifact: %w", err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("close deployment source artifact: %w", err)
		}
		return path, cleanup, nil
	}
}

func readLimitedFormField(part *multipart.Part, limit int64) (string, error) {
	defer part.Close()
	bytes, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(bytes)) > limit {
		return "", errors.New("field is too large")
	}
	return string(bytes), nil
}

func (s *Server) deleteUnreferencedDeploymentSourceArtifact(ctx context.Context, digest string) {
	digest = strings.TrimSpace(digest)
	if digest == "" || s.cas == nil {
		return
	}
	if store, ok := s.db.(casObjectLookupStore); ok {
		if _, err := store.GetCasObject(ctx, digest); err == nil {
			return
		} else if !isNoRows(err) {
			if s.log != nil {
				s.log.Warn("skip deployment source artifact cleanup after CAS lookup failure", "digest", digest, "error", err)
			}
			return
		}
	}
	if err := s.cas.Delete(ctx, digest); err != nil && s.log != nil {
		s.log.Warn("delete unreferenced deployment source artifact", "digest", digest, "error", err)
	}
}

func validateDeploymentSourceArtifactArchive(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open deployment source artifact: %w", err)
	}
	defer file.Close()
	destination, err := os.MkdirTemp("", "helmr-deployment-source-validate-*")
	if err != nil {
		return fmt.Errorf("create deployment source validation directory: %w", err)
	}
	defer os.RemoveAll(destination)
	if err := archive.ExtractTar(file, destination); err != nil {
		return err
	}
	return nil
}

func deploymentArchiveDigest(archivePath string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type artifactLister interface {
	ListArtifactsByIDs(context.Context, db.ListArtifactsByIDsParams) ([]db.Artifact, error)
}

func deploymentResponseWithArtifacts(ctx context.Context, store artifactLister, deployment db.Deployment) (api.DeploymentResponse, error) {
	idsToResolve := []pgtype.UUID{deployment.DeploymentSourceArtifactID}
	if deployment.BuildManifestArtifactID.Valid {
		idsToResolve = append(idsToResolve, deployment.BuildManifestArtifactID)
	}
	if deployment.DeploymentManifestArtifactID.Valid {
		idsToResolve = append(idsToResolve, deployment.DeploymentManifestArtifactID)
	}
	artifacts, err := scopedArtifactsByID(ctx, store, deployment.OrgID, deployment.ProjectID, deployment.EnvironmentID, idsToResolve)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	sourceArtifact, err := deploymentSourceArtifact(artifacts, deployment.DeploymentSourceArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	buildManifestDigest, err := optionalDeploymentArtifactDigest(artifacts, deployment.BuildManifestArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	deploymentManifestDigest, err := optionalDeploymentArtifactDigest(artifacts, deployment.DeploymentManifestArtifactID)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return deploymentResponse(deployment, sourceArtifact, buildManifestDigest, deploymentManifestDigest), nil
}

func deploymentSourceArtifact(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (api.DeploymentSourceArtifact, error) {
	artifact, err := requiredArtifact(artifacts, artifactID)
	if err != nil {
		return api.DeploymentSourceArtifact{}, err
	}
	return api.DeploymentSourceArtifact{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}, nil
}

func optionalDeploymentArtifactDigest(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (string, error) {
	if !artifactID.Valid {
		return "", nil
	}
	artifact, err := requiredArtifact(artifacts, artifactID)
	if err != nil {
		return "", err
	}
	return artifact.Digest, nil
}

func scopedArtifactsByID(ctx context.Context, store artifactLister, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, artifactIDs []pgtype.UUID) (map[pgtype.UUID]db.Artifact, error) {
	unique := make([]pgtype.UUID, 0, len(artifactIDs))
	seen := map[pgtype.UUID]struct{}{}
	for _, artifactID := range artifactIDs {
		if !artifactID.Valid {
			continue
		}
		if _, ok := seen[artifactID]; ok {
			continue
		}
		seen[artifactID] = struct{}{}
		unique = append(unique, artifactID)
	}
	if len(unique) == 0 {
		return map[pgtype.UUID]db.Artifact{}, nil
	}
	rows, err := store.ListArtifactsByIDs(ctx, db.ListArtifactsByIDsParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Ids:           unique,
	})
	if err != nil {
		return nil, err
	}
	artifacts := make(map[pgtype.UUID]db.Artifact, len(rows))
	for _, artifact := range rows {
		artifacts[artifact.ID] = artifact
	}
	for _, artifactID := range unique {
		if _, ok := artifacts[artifactID]; !ok {
			return nil, errRecordNotFound
		}
	}
	return artifacts, nil
}

func requiredArtifact(artifacts map[pgtype.UUID]db.Artifact, artifactID pgtype.UUID) (db.Artifact, error) {
	artifact, ok := artifacts[artifactID]
	if !ok {
		return db.Artifact{}, errRecordNotFound
	}
	return artifact, nil
}
