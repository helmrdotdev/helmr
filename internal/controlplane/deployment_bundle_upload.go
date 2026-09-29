package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const deploymentBundleUploadExpiry = 15 * time.Minute

type deploymentBundleOwnershipStore interface {
	GetCasObject(context.Context, db.GetCasObjectParams) (db.CasObject, error)
}

func (s *Server) planDeploymentBundleUpload(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != bundle.MediaType || len(parameters) != 0 {
		writeError(w, badRequest(errors.New("deployment bundle Content-Type is invalid")))
		return
	}
	manifest, raw, err := parseRequestBody(r, bundle.Parse)
	if err != nil {
		writeError(w, fmt.Errorf("invalid deployment bundle: %w", err))
		return
	}
	if err := s.bundleAdmission.Admit(manifest); err != nil {
		writeError(w, badRequest(err))
		return
	}

	response, err := planDeploymentBundleUploads(
		r.Context(), s.cas, s.db, s.platformStore,
		strings.ToLower(actor.OrgID.String()), pgvalue.UUID(actor.OrgID), raw, manifest,
	)
	if err != nil {
		s.writeDeploymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func planDeploymentBundleUploads(
	ctx context.Context,
	uploads cas.UploadStore,
	ownership deploymentBundleOwnershipStore,
	platform cas.Reader,
	owner string,
	orgID pgtype.UUID,
	raw []byte,
	manifest bundle.Manifest,
) (api.DeploymentBundleUploadPlanResponse, error) {
	runtimeExpected := cas.Descriptor{
		Digest: manifest.Runtime.Artifact.Digest, SizeBytes: manifest.Runtime.Artifact.SizeBytes,
		MediaType: manifest.Runtime.Artifact.MediaType,
	}
	runtimeObject, err := platform.Stat(ctx, runtimeExpected.Digest)
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve supported Runtime object: %w", err)
	}
	if err := cas.RequireExact(runtimeObject, runtimeExpected); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("supported Runtime object: %w", err)
	}

	bundleDigest, err := bundle.Digest(raw)
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, err
	}
	root := cas.Descriptor{
		Digest: bundleDigest, SizeBytes: int64(len(raw)), MediaType: bundle.MediaType,
	}
	if err := uploads.PutQuarantine(ctx, owner, root, bytes.NewReader(raw)); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("quarantine deployment bundle root: %w", err)
	}
	if _, err := uploads.PromoteQuarantine(ctx, owner, root); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("publish deployment bundle root: %w", err)
	}

	response := api.DeploymentBundleUploadPlanResponse{
		BundleDigest: bundleDigest,
		Uploads:      make([]api.DeploymentBundleUpload, 0, len(manifest.Objects)),
	}
	for _, object := range manifest.Objects {
		descriptor := cas.Descriptor{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}
		owned, err := ownership.GetCasObject(ctx, db.GetCasObjectParams{
			OrgID: orgID, Digest: object.Digest,
		})
		if err == nil {
			if owned.SizeBytes != descriptor.SizeBytes || owned.MediaType != descriptor.MediaType {
				return api.DeploymentBundleUploadPlanResponse{}, errors.New("owned deployment object descriptor conflicts with bundle")
			}
			global, statErr := uploads.Stat(ctx, descriptor.Digest)
			if statErr != nil {
				return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve owned deployment object: %w", statErr)
			}
			if err := cas.RequireExact(global, descriptor); err != nil {
				return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("owned deployment object: %w", err)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve deployment object ownership: %w", err)
		}
		hasExactQuarantine, err := uploads.HasExactQuarantine(ctx, owner, descriptor)
		if err != nil {
			return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve quarantined deployment object: %w", err)
		}
		if hasExactQuarantine {
			continue
		}
		presigned, err := uploads.PresignQuarantine(ctx, owner, descriptor, deploymentBundleUploadExpiry)
		if err != nil {
			return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("plan deployment object upload: %w", err)
		}
		response.Uploads = append(response.Uploads, api.DeploymentBundleUpload{
			Digest: descriptor.Digest, Method: presigned.Method, URL: presigned.URL,
			Headers: cloneStringMap(presigned.Headers),
		})
	}
	return response, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
