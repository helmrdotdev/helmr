package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const deploymentBundleUploadExpiry = 15 * time.Minute

type deploymentBundleUploadStore interface {
	Stat(context.Context, string) (cas.Object, error)
	PutQuarantine(context.Context, string, cas.Descriptor, io.Reader) error
	PresignQuarantine(context.Context, string, cas.Descriptor, time.Duration) (cas.PresignedUpload, error)
}

type deploymentBundleOwnershipStore interface {
	GetCasObject(context.Context, db.GetCasObjectParams) (db.CasObject, error)
}

func (s *Server) planDeploymentBundleUpload(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || s.bundleAdmission == nil || s.platformStore == nil {
		writeError(w, unavailable(errors.New("deployment bundle admission is not configured")))
		return
	}
	uploads, ok := s.cas.(deploymentBundleUploadStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment bundle upload storage is not configured")))
		return
	}
	ownership, ok := s.db.(deploymentBundleOwnershipStore)
	if !ok {
		writeError(w, unavailable(errors.New("deployment bundle ownership storage is not configured")))
		return
	}
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
	if err != nil || mediaType != deployment.DeploymentBundleMediaType || len(parameters) != 0 {
		writeError(w, badRequest(errors.New("deployment bundle Content-Type is invalid")))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, deployment.MaxDeploymentBundleBytes+1))
	if err != nil {
		writeError(w, badRequest(errors.New("read deployment bundle")))
		return
	}
	bundle, err := deployment.ParseDeploymentBundle(raw)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment bundle: %w", err)))
		return
	}
	if err := s.bundleAdmission.Admit(bundle); err != nil {
		writeError(w, badRequest(err))
		return
	}

	response, err := planDeploymentBundleUploads(
		r.Context(), uploads, ownership, s.platformStore,
		strings.ToLower(actor.OrgID.String()), pgvalue.UUID(actor.OrgID), raw, bundle,
	)
	if err != nil {
		writeDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func planDeploymentBundleUploads(
	ctx context.Context,
	uploads deploymentBundleUploadStore,
	ownership deploymentBundleOwnershipStore,
	platform cas.Reader,
	owner string,
	orgID pgtype.UUID,
	raw []byte,
	bundle deployment.DeploymentBundle,
) (api.DeploymentBundleUploadPlanResponse, error) {
	if uploads == nil || ownership == nil || platform == nil {
		return api.DeploymentBundleUploadPlanResponse{}, errors.New("deployment bundle upload dependencies are incomplete")
	}
	runtimeExpected := cas.Descriptor{
		Digest: bundle.Runtime.Artifact.Digest, SizeBytes: bundle.Runtime.Artifact.SizeBytes,
		MediaType: bundle.Runtime.Artifact.MediaType,
	}
	runtimeObject, err := platform.Stat(ctx, runtimeExpected.Digest)
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve supported Runtime object: %w", err)
	}
	if err := requireExactCASObject(runtimeObject, runtimeExpected); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("supported Runtime object: %w", err)
	}

	bundleDigest, err := deployment.DeploymentBundleDigest(raw)
	if err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, err
	}
	root := cas.Descriptor{
		Digest: bundleDigest, SizeBytes: int64(len(raw)), MediaType: deployment.DeploymentBundleMediaType,
	}
	if err := uploads.PutQuarantine(ctx, owner, root, bytes.NewReader(raw)); err != nil {
		return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("quarantine deployment bundle root: %w", err)
	}

	response := api.DeploymentBundleUploadPlanResponse{
		BundleDigest: bundleDigest,
		Uploads:      make([]api.DeploymentBundleUpload, 0, len(bundle.Objects)),
	}
	for _, object := range bundle.Objects {
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
			if err := requireExactCASObject(global, descriptor); err != nil {
				return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("owned deployment object: %w", err)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.DeploymentBundleUploadPlanResponse{}, fmt.Errorf("resolve deployment object ownership: %w", err)
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

func requireExactCASObject(object cas.Object, expected cas.Descriptor) error {
	if object.Digest != expected.Digest || object.SizeBytes != expected.SizeBytes || object.MediaType != expected.MediaType {
		return errors.New("CAS object does not match its descriptor")
	}
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
