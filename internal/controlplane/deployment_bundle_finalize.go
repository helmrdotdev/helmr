package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type finalizedDeploymentBundle struct {
	root        cas.Descriptor
	bundle      deployment.DeploymentBundle
	objects     []cas.Descriptor
	definitions []finalizedDeploymentDefinition
	queueConfig []byte
	indexDigest []byte
}

type finalizedDeploymentDefinition struct {
	kind           string
	declaredID     string
	manifest       []byte
	manifestDigest []byte
	artifact       *cas.Descriptor
}

type deploymentFinalizeReceipt struct {
	DeploymentID string `json:"deploymentId"`
}

func (s *Server) finalizeDeploymentBundle(w http.ResponseWriter, r *http.Request) {
	uploads, ok := s.cas.(cas.UploadStore)
	if !ok || s.bundleAdmission == nil || s.platformStore == nil {
		writeError(w, unavailable(errors.New("deployment bundle finalization is not configured")))
		return
	}
	var request api.FinalizeDeploymentBundleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid deployment bundle finalization request: %w", err)))
		return
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.BundleDigest = strings.TrimSpace(request.BundleDigest)
	if request.IdempotencyKey == "" {
		writeError(w, badRequest(errors.New("deployment idempotency key is required")))
		return
	}
	if _, err := deployment.RuntimeDigestBytes(request.BundleDigest); err != nil {
		writeError(w, badRequest(errors.New("deployment bundle digest is invalid")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	prepared, err := s.prepareFinalizedDeploymentBundle(
		r.Context(), uploads, strings.ToLower(actor.OrgID.String()), request.BundleDigest,
	)
	if err != nil {
		writeDeploymentError(w, s, badRequest(err))
		return
	}

	idempotencyRequest, err := idempotency.NewDeploymentFinalizeRequest(
		pgvalue.MustUUIDValue(environmentID), pgvalue.MustUUIDValue(projectID),
		request.IdempotencyKey,
		idempotency.DeploymentFinalizeFingerprint{BundleDigest: request.BundleDigest},
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var response api.DeploymentResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		claim, err := claims.Acquire(r.Context(), idempotencyRequest)
		if err != nil {
			return err
		}
		if !claim.New {
			return replayFinalizedDeployment(
				r.Context(), work.q, claim.Claim,
				pgvalue.UUID(actor.OrgID), projectID, &response,
			)
		}
		if err := work.q.LockDeploymentBundle(r.Context(), db.LockDeploymentBundleParams{
			EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
		}); err != nil {
			return fmt.Errorf("lock deployment bundle: %w", err)
		}
		existing, err := work.q.GetDeploymentByBundleDigest(
			r.Context(), db.GetDeploymentByBundleDigestParams{
				EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
			},
		)
		if err == nil {
			response = deploymentResponse(existing)
			return completeDeploymentFinalization(r.Context(), claims, claim.Claim, response.ID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("resolve deployment bundle: %w", err)
		}
		created, err := createFinalizedDeployment(
			r.Context(), work.q, pgvalue.UUID(actor.OrgID), projectID, environmentID, prepared,
		)
		if err != nil {
			return err
		}
		response = deploymentResponse(created)
		return completeDeploymentFinalization(r.Context(), claims, claim.Claim, response.ID)
	})
	if err != nil {
		writeDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) prepareFinalizedDeploymentBundle(
	ctx context.Context,
	uploads cas.UploadStore,
	owner string,
	bundleDigest string,
) (finalizedDeploymentBundle, error) {
	rootObject, err := uploads.Stat(ctx, bundleDigest)
	if err != nil {
		return finalizedDeploymentBundle{}, fmt.Errorf("resolve deployment bundle root: %w", err)
	}
	if rootObject.MediaType != deployment.DeploymentBundleMediaType ||
		rootObject.SizeBytes < 1 || rootObject.SizeBytes > deployment.MaxDeploymentBundleBytes {
		return finalizedDeploymentBundle{}, errors.New("deployment bundle root descriptor is invalid")
	}
	rootReader, err := uploads.Get(ctx, bundleDigest)
	if err != nil {
		return finalizedDeploymentBundle{}, fmt.Errorf("read deployment bundle root: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(rootReader, deployment.MaxDeploymentBundleBytes+1))
	closeErr := rootReader.Close()
	if readErr != nil || closeErr != nil {
		return finalizedDeploymentBundle{}, errors.Join(readErr, closeErr)
	}
	actualDigest, err := deployment.DeploymentBundleDigest(raw)
	if err != nil || actualDigest != bundleDigest || int64(len(raw)) != rootObject.SizeBytes {
		return finalizedDeploymentBundle{}, errors.New("deployment bundle root bytes do not match the request")
	}
	bundle, err := deployment.ParseDeploymentBundle(raw)
	if err != nil {
		return finalizedDeploymentBundle{}, err
	}
	if err := s.bundleAdmission.Admit(bundle); err != nil {
		return finalizedDeploymentBundle{}, err
	}
	if err := requireSupportedRuntime(ctx, s.platformStore, bundle.Runtime.Artifact); err != nil {
		return finalizedDeploymentBundle{}, err
	}
	objects := make([]cas.Descriptor, 0, len(bundle.Objects))
	for _, object := range bundle.Objects {
		descriptor := cas.Descriptor{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}
		owned, ownershipErr := s.db.GetCasObject(ctx, db.GetCasObjectParams{
			OrgID: pgvalue.UUID(actorFromContext(ctx).OrgID), Digest: descriptor.Digest,
		})
		switch {
		case ownershipErr == nil:
			if owned.SizeBytes != descriptor.SizeBytes || owned.MediaType != descriptor.MediaType {
				return finalizedDeploymentBundle{}, errors.New("owned deployment object descriptor conflicts with bundle")
			}
			object, statErr := uploads.Stat(ctx, descriptor.Digest)
			if statErr != nil || requireExactCASObject(object, descriptor) != nil {
				return finalizedDeploymentBundle{}, errors.New("owned deployment object is unavailable")
			}
		case errors.Is(ownershipErr, pgx.ErrNoRows):
			object, promoteErr := uploads.PromoteQuarantine(ctx, owner, descriptor)
			if promoteErr != nil {
				return finalizedDeploymentBundle{}, fmt.Errorf("publish deployment object %s: %w", descriptor.Digest, promoteErr)
			}
			if err := requireExactCASObject(object, descriptor); err != nil {
				return finalizedDeploymentBundle{}, err
			}
		default:
			return finalizedDeploymentBundle{}, fmt.Errorf("resolve deployment object ownership: %w", ownershipErr)
		}
		objects = append(objects, descriptor)
	}
	if err := verifyFinalizedDeploymentObjects(ctx, uploads, bundle); err != nil {
		return finalizedDeploymentBundle{}, err
	}
	definitions, err := finalizedDeploymentDefinitions(bundle)
	if err != nil {
		return finalizedDeploymentBundle{}, err
	}
	queueConfig, err := canonicalDeploymentQueueConfig(bundle.Plan)
	if err != nil {
		return finalizedDeploymentBundle{}, err
	}
	index, err := deployment.CanonicalProgramIndex(bundle.Program.Index)
	if err != nil {
		return finalizedDeploymentBundle{}, err
	}
	indexDigest := sha256.Sum256(index)
	return finalizedDeploymentBundle{
		root:   cas.Descriptor{Digest: bundleDigest, SizeBytes: rootObject.SizeBytes, MediaType: rootObject.MediaType},
		bundle: bundle, objects: objects, definitions: definitions,
		queueConfig: queueConfig, indexDigest: indexDigest[:],
	}, nil
}

func requireSupportedRuntime(ctx context.Context, store cas.Reader, runtime deployment.BundleObject) error {
	object, err := store.Stat(ctx, runtime.Digest)
	if err != nil {
		return fmt.Errorf("resolve supported Runtime object: %w", err)
	}
	if err := requireExactCASObject(object, cas.Descriptor{
		Digest: runtime.Digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType,
	}); err != nil {
		return fmt.Errorf("supported Runtime object: %w", err)
	}
	return nil
}

func verifyFinalizedDeploymentObjects(ctx context.Context, store cas.Reader, bundle deployment.DeploymentBundle) error {
	for _, object := range bundle.Objects {
		reader, err := store.Get(ctx, object.Digest)
		if err != nil {
			return fmt.Errorf("read deployment object %s: %w", object.Digest, err)
		}
		switch object.MediaType {
		case deployment.ProgramArtifactMediaType:
			err = verifyStoredProgram(ctx, reader, bundle.Program)
		case deployment.WorkspaceImageArtifactMediaType:
			var metadata oci.Metadata
			metadata, err = oci.Inspect(io.LimitReader(reader, object.SizeBytes+1))
			if err == nil && (metadata.ManifestCount != 1 || metadata.Platform == nil ||
				metadata.Platform.OS != deployment.DeploymentBundleTargetOS || metadata.Platform.Architecture != "amd64") {
				err = errors.New("workspace image platform does not match linux/amd64")
			}
		default:
			err = errors.New("deployment object media type is unsupported")
		}
		closeErr := reader.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("verify deployment object %s: %w", object.Digest, errors.Join(err, closeErr))
		}
	}
	return nil
}

func verifyStoredProgram(ctx context.Context, source io.Reader, program deployment.ProgramOutput) (returnErr error) {
	file, err := os.CreateTemp("", "helmr-program-admission-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { returnErr = errors.Join(returnErr, file.Close(), os.Remove(name)) }()
	if _, err := io.Copy(file, io.LimitReader(source, program.Artifact.SizeBytes+1)); err != nil {
		return err
	}
	return deployment.VerifyProgramOutputFile(ctx, file, program)
}

func finalizedDeploymentDefinitions(bundle deployment.DeploymentBundle) ([]finalizedDeploymentDefinition, error) {
	images := make(map[string]cas.Descriptor, len(bundle.WorkspaceImages))
	for _, image := range bundle.WorkspaceImages {
		descriptor := image.Artifact
		images[image.DeclaredID] = cas.Descriptor{
			Digest: descriptor.Digest, SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType,
		}
	}
	definitions := make([]finalizedDeploymentDefinition, 0, len(bundle.Plan.Definitions))
	for _, definition := range bundle.Plan.Definitions {
		var manifest any
		var artifact *cas.Descriptor
		switch definition.Kind {
		case deployment.DefinitionKindTask:
			manifest = definition.Task
		case deployment.DefinitionKindActor:
			manifest = definition.Actor
		case deployment.DefinitionKindSandbox:
			manifest = definition.Sandbox
			value, ok := images[definition.DeclaredID]
			if !ok {
				return nil, fmt.Errorf("deployment sandbox %q has no image", definition.DeclaredID)
			}
			artifact = &value
		default:
			return nil, fmt.Errorf("deployment definition kind %q is unsupported", definition.Kind)
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		canonical, digest, err := deployment.CanonicalManifestAndDigest(raw)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, finalizedDeploymentDefinition{
			kind: string(definition.Kind), declaredID: definition.DeclaredID,
			manifest: canonical, manifestDigest: digest[:], artifact: artifact,
		})
	}
	return definitions, nil
}

func canonicalDeploymentQueueConfig(plan deployment.DeploymentPlan) ([]byte, error) {
	queues := make([]deployment.QueueInput, len(plan.Queues))
	for index, queue := range plan.Queues {
		queues[index] = queue
		if queue.ConcurrencyLimit != nil {
			value := *queue.ConcurrencyLimit
			queues[index].ConcurrencyLimit = &value
		}
	}
	return deployment.CanonicalQueueConfig(deployment.QueueConfig{
		FormatVersion: deployment.DeploymentPlanFormatVersion, Queues: queues,
	})
}

func createFinalizedDeployment(
	ctx context.Context,
	queries db.Querier,
	orgID, projectID, environmentID pgtype.UUID,
	prepared finalizedDeploymentBundle,
) (db.Deployment, error) {
	for _, object := range append([]cas.Descriptor{prepared.root}, prepared.objects...) {
		if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
			OrgID: orgID, Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}); err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment CAS ownership: %w", err)
		}
	}
	artifacts := make(map[string]db.Artifact, len(prepared.objects))
	for _, object := range prepared.objects {
		kind := db.ArtifactKindWorkspaceImage
		if object.MediaType == deployment.ProgramArtifactMediaType {
			kind = db.ArtifactKindDeploymentProgram
		}
		artifact, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: orgID, ProjectID: projectID,
			EnvironmentID: environmentID, Digest: object.Digest, Kind: kind,
			SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		})
		if err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment artifact: %w", err)
		}
		artifacts[object.Digest] = artifact
	}
	programArtifact := artifacts[prepared.bundle.Program.Artifact.Digest]
	deploymentID := uuid.Must(uuid.NewV7())
	record, err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: pgvalue.UUID(deploymentID), OrgID: orgID, ProjectID: projectID,
		EnvironmentID: environmentID, Version: deploymentVersion(deploymentID),
		BundleDigest:          prepared.root.Digest,
		RuntimeArtifactDigest: prepared.bundle.Runtime.Artifact.Digest,
		ProgramArtifactID:     programArtifact.ID, ProgramIndexDigest: prepared.indexDigest,
		QueueConfig: prepared.queueConfig,
	})
	if err != nil {
		return db.Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	for _, definition := range prepared.definitions {
		var artifactID pgtype.UUID
		if definition.artifact != nil {
			artifactID = artifacts[definition.artifact.Digest].ID
		}
		if _, err := queries.CreateDeploymentDefinition(ctx, db.CreateDeploymentDefinitionParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), EnvironmentID: environmentID,
			DeploymentID: record.ID, Kind: definition.kind, DeclaredID: definition.declaredID,
			ManifestVersion: deployment.DeploymentPlanFormatVersion,
			Manifest:        definition.manifest, ManifestDigest: definition.manifestDigest,
			ArtifactID: artifactID,
		}); err != nil {
			return db.Deployment{}, fmt.Errorf("create deployment definition: %w", err)
		}
	}
	return record, nil
}

func completeDeploymentFinalization(
	ctx context.Context,
	claims *idempotency.Transaction,
	claim db.IdempotencyClaim,
	deploymentID string,
) error {
	receipt, err := json.Marshal(deploymentFinalizeReceipt{DeploymentID: deploymentID})
	if err != nil {
		return err
	}
	_, err = claims.Complete(ctx, claim, receipt)
	return err
}

func replayFinalizedDeployment(
	ctx context.Context,
	queries db.Querier,
	claim db.IdempotencyClaim,
	orgID pgtype.UUID,
	projectID pgtype.UUID,
	response *api.DeploymentResponse,
) error {
	if claim.State != "completed" {
		return conflict(errors.New("deployment bundle finalization is in progress"))
	}
	var receipt deploymentFinalizeReceipt
	decoder := json.NewDecoder(strings.NewReader(string(claim.Receipt)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || receipt.DeploymentID == "" {
		return errors.New("deployment finalization receipt is invalid")
	}
	id, err := uuid.Parse(receipt.DeploymentID)
	if err != nil {
		return errors.New("deployment finalization receipt is invalid")
	}
	record, err := queries.GetDeployment(ctx, db.GetDeploymentParams{
		OrgID: orgID, ProjectID: projectID,
		EnvironmentID: claim.EnvironmentID, ID: pgvalue.UUID(id),
	})
	if err != nil {
		return err
	}
	*response = deploymentResponse(record)
	return nil
}
