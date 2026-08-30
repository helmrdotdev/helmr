package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"uuid"

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

type deploymentFinalizeProgress struct {
	digest string
}

type deploymentFinalizeResult struct {
	response api.DeploymentResponse
	err      error
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
		r.Context(), uploads, request.BundleDigest,
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
	s.streamFinalizedDeploymentBundle(
		w, r, uploads, actor.OrgID, projectID, environmentID, prepared, idempotencyRequest,
	)
}

func (s *Server) finishFinalizedDeploymentBundle(
	ctx context.Context,
	uploads cas.UploadStore,
	orgID uuid.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	prepared finalizedDeploymentBundle,
	idempotencyRequest idempotency.Request,
	progress func(deploymentFinalizeProgress) error,
) (api.DeploymentResponse, error) {
	replay, err := s.finalizedDeploymentAvailable(ctx, uploads, environmentID, prepared)
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	if replay {
		return s.registerFinalizedDeploymentBundle(
			ctx, orgID, projectID, environmentID, prepared, idempotencyRequest,
		)
	}
	if err := s.verifyFinalizedDeploymentObjects(ctx, uploads, orgID, prepared, progress); err != nil {
		return api.DeploymentResponse{}, err
	}
	return s.registerFinalizedDeploymentBundle(
		ctx, orgID, projectID, environmentID, prepared, idempotencyRequest,
	)
}

func (s *Server) finalizedDeploymentAvailable(
	ctx context.Context,
	store cas.Reader,
	environmentID pgtype.UUID,
	prepared finalizedDeploymentBundle,
) (bool, error) {
	_, err := s.db.GetDeploymentByBundleDigest(ctx, db.GetDeploymentByBundleDigestParams{
		EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve deployment bundle replay: %w", err)
	}
	for _, descriptor := range prepared.objects {
		object, err := store.Stat(ctx, descriptor.Digest)
		if err != nil || requireExactCASObject(object, descriptor) != nil {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) registerFinalizedDeploymentBundle(
	ctx context.Context,
	orgID uuid.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	prepared finalizedDeploymentBundle,
	idempotencyRequest idempotency.Request,
) (api.DeploymentResponse, error) {
	var response api.DeploymentResponse
	err := s.inTx(ctx, func(work *txWork) error {
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		claim, err := claims.Acquire(ctx, idempotencyRequest)
		if err != nil {
			return err
		}
		if !claim.New {
			return replayFinalizedDeployment(
				ctx, work.q, claim.Claim, pgvalue.UUID(orgID), projectID, &response,
			)
		}
		if err := work.q.LockDeploymentBundle(ctx, db.LockDeploymentBundleParams{
			EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
		}); err != nil {
			return fmt.Errorf("lock deployment bundle: %w", err)
		}
		existing, err := work.q.GetDeploymentByBundleDigest(
			ctx, db.GetDeploymentByBundleDigestParams{
				EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
			},
		)
		if err == nil {
			response = deploymentResponse(existing)
			return completeDeploymentFinalization(ctx, claims, claim.Claim, response.ID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("resolve deployment bundle: %w", err)
		}
		created, err := createFinalizedDeployment(
			ctx, work.q, pgvalue.UUID(orgID), projectID, environmentID, prepared,
		)
		if err != nil {
			return err
		}
		response = deploymentResponse(created)
		return completeDeploymentFinalization(ctx, claims, claim.Claim, response.ID)
	})
	return response, err
}

func (s *Server) streamFinalizedDeploymentBundle(
	w http.ResponseWriter,
	r *http.Request,
	uploads cas.UploadStore,
	orgID uuid.UUID,
	projectID pgtype.UUID,
	environmentID pgtype.UUID,
	prepared finalizedDeploymentBundle,
	idempotencyRequest idempotency.Request,
) {
	s.streamDeploymentFinalization(w, r, prepared.root.Digest, func(
		ctx context.Context,
		progress func(deploymentFinalizeProgress) error,
	) (api.DeploymentResponse, error) {
		return s.finishFinalizedDeploymentBundle(
			ctx, uploads, orgID, projectID, environmentID, prepared, idempotencyRequest, progress,
		)
	})
}

type deploymentFinalizer func(
	context.Context,
	func(deploymentFinalizeProgress) error,
) (api.DeploymentResponse, error)

func (s *Server) streamDeploymentFinalization(
	w http.ResponseWriter,
	r *http.Request,
	bundleDigest string,
	finish deploymentFinalizer,
) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, unavailable(errors.New("deployment finalization streaming is unavailable")))
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeDeploymentFinalizeEvent(w, api.DeploymentBundleFinalizeEventStarted, api.DeploymentBundleFinalizeStarted{
		BundleDigest: bundleDigest,
	}); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	progress := make(chan deploymentFinalizeProgress)
	result := make(chan deploymentFinalizeResult, 1)
	go func() {
		var completed deploymentFinalizeResult
		defer func() {
			if recovered := recover(); recovered != nil {
				if s.log != nil {
					s.log.ErrorContext(ctx, "deployment finalizer panic", "panic", recovered, "stack", string(debug.Stack()))
				}
				completed = deploymentFinalizeResult{err: errors.New("deployment finalizer panicked")}
			}
			result <- completed
		}()
		completed.response, completed.err = finish(ctx, func(update deploymentFinalizeProgress) error {
			select {
			case progress <- update:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	pingEvery := s.deploymentFinalizePingEvery
	if pingEvery <= 0 {
		pingEvery = 10 * time.Second
	}
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		var event string
		var payload any
		terminal := false
		select {
		case <-r.Context().Done():
			if s.log != nil {
				s.log.Info("deployment finalization stream disconnected", "bundle_digest", bundleDigest)
			}
			return
		case <-ticker.C:
			event = api.DeploymentBundleFinalizeEventPing
			payload = struct{}{}
		case update := <-progress:
			event = api.DeploymentBundleFinalizeEventObjectVerified
			payload = api.DeploymentBundleFinalizeObject{Digest: update.digest}
		case completed := <-result:
			terminal = true
			if completed.err == nil {
				event = api.DeploymentBundleFinalizeEventComplete
				payload = completed.response
			} else {
				if s.log != nil {
					s.log.Error("deployment bundle finalization failed", "error", completed.err)
				}
				event = api.DeploymentBundleFinalizeEventError
				payload = publicDeploymentFinalizeError(completed.err)
			}
		}
		if err := writeDeploymentFinalizeEvent(w, event, payload); err != nil {
			cancel()
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			cancel()
			return
		}
		if terminal {
			return
		}
	}
}

func writeDeploymentFinalizeEvent(w io.Writer, event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if bytes.ContainsAny(encoded, "\r\n") {
		return errors.New("deployment finalization event is not a single line")
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
	return err
}

type invalidDeploymentObjectError struct{ err error }

func (e invalidDeploymentObjectError) Error() string { return e.err.Error() }
func (e invalidDeploymentObjectError) Unwrap() error { return e.err }

type deploymentObjectSourceError struct{ err error }

func (e deploymentObjectSourceError) Error() string { return e.err.Error() }
func (e deploymentObjectSourceError) Unwrap() error { return e.err }

func publicDeploymentFinalizeError(err error) api.DeploymentBundleFinalizeError {
	var idempotencyConflict idempotency.ConflictError
	if errors.As(err, &idempotencyConflict) {
		return api.DeploymentBundleFinalizeError{
			Code: "idempotency_conflict", Message: "idempotency key conflicts with another deployment bundle",
		}
	}
	var invalidObject invalidDeploymentObjectError
	if errors.As(err, &invalidObject) {
		return api.DeploymentBundleFinalizeError{
			Code: "invalid_deployment_object", Message: "deployment object failed verification",
		}
	}
	return api.DeploymentBundleFinalizeError{
		Code: "deployment_finalization_unavailable", Message: "deployment finalization is unavailable",
	}
}

func (s *Server) prepareFinalizedDeploymentBundle(
	ctx context.Context,
	uploads cas.UploadStore,
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
		objects = append(objects, cas.Descriptor{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		})
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

func (s *Server) verifyFinalizedDeploymentObjects(
	ctx context.Context,
	uploads cas.UploadStore,
	orgID uuid.UUID,
	prepared finalizedDeploymentBundle,
	progress func(deploymentFinalizeProgress) error,
) error {
	startedAt := time.Now()
	var verifiedBytes int64
	owner := strings.ToLower(orgID.String())
	for _, object := range prepared.objects {
		if err := s.requireFinalizedDeploymentObject(ctx, uploads, owner, orgID, object); err != nil {
			return err
		}
		if err := s.verifyFinalizedDeploymentObject(ctx, uploads, prepared.bundle, object); err != nil {
			return err
		}
		if err := progress(deploymentFinalizeProgress{digest: object.Digest}); err != nil {
			return err
		}
		verifiedBytes += object.SizeBytes
	}
	if s.log != nil {
		s.log.Info("deployment objects verified",
			"bundle_digest", prepared.root.Digest,
			"object_count", len(prepared.objects),
			"verified_bytes", verifiedBytes,
			"duration", time.Since(startedAt),
		)
	}
	return nil
}

func (s *Server) requireFinalizedDeploymentObject(
	ctx context.Context,
	uploads cas.UploadStore,
	owner string,
	orgID uuid.UUID,
	descriptor cas.Descriptor,
) error {
	owned, ownershipErr := s.db.GetCasObject(ctx, db.GetCasObjectParams{
		OrgID: pgvalue.UUID(orgID), Digest: descriptor.Digest,
	})
	switch {
	case ownershipErr == nil:
		if owned.SizeBytes != descriptor.SizeBytes || owned.MediaType != descriptor.MediaType {
			return invalidDeploymentObjectError{err: errors.New("owned deployment object descriptor conflicts with bundle")}
		}
		stored, statErr := uploads.Stat(ctx, descriptor.Digest)
		if statErr != nil {
			return fmt.Errorf("owned deployment object is unavailable: %w", statErr)
		}
		if err := requireExactCASObject(stored, descriptor); err != nil {
			return fmt.Errorf("owned deployment object is unavailable: %w", err)
		}
	case errors.Is(ownershipErr, pgx.ErrNoRows):
		stored, promoteErr := uploads.PromoteQuarantine(ctx, owner, descriptor)
		if promoteErr != nil {
			return fmt.Errorf("publish deployment object %s: %w", descriptor.Digest, promoteErr)
		}
		if err := requireExactCASObject(stored, descriptor); err != nil {
			return err
		}
	default:
		return fmt.Errorf("resolve deployment object ownership: %w", ownershipErr)
	}
	return nil
}

func (s *Server) verifyFinalizedDeploymentObject(
	ctx context.Context,
	store cas.Reader,
	bundle deployment.DeploymentBundle,
	object cas.Descriptor,
) error {
	if s.deploymentVerifierSlots != nil {
		select {
		case s.deploymentVerifierSlots <- struct{}{}:
			defer func() { <-s.deploymentVerifierSlots }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	reader, err := store.Get(ctx, object.Digest)
	if err != nil {
		return fmt.Errorf("read deployment object %s: %w", object.Digest, err)
	}
	recorded := &deploymentObjectReader{source: reader}
	switch object.MediaType {
	case deployment.ProgramArtifactMediaType:
		err = verifyStoredProgram(ctx, recorded, bundle.Program)
	case deployment.WorkspaceImageArtifactMediaType:
		var metadata oci.Metadata
		metadata, err = oci.Inspect(io.LimitReader(recorded, object.SizeBytes+1))
		if err == nil && (metadata.ManifestCount != 1 || metadata.Platform == nil ||
			metadata.Platform.OS != deployment.DeploymentBundleTargetOS || metadata.Platform.Architecture != "amd64") {
			err = errors.New("workspace image platform does not match linux/amd64")
		}
	default:
		err = errors.New("deployment object media type is unsupported")
	}
	closeErr := reader.Close()
	if recorded.err != nil || closeErr != nil {
		return fmt.Errorf("read deployment object %s: %w", object.Digest, errors.Join(recorded.err, closeErr))
	}
	if err != nil {
		var sourceErr deploymentObjectSourceError
		if errors.As(err, &sourceErr) {
			return fmt.Errorf("read deployment object %s: %w", object.Digest, err)
		}
		var invalidErr invalidDeploymentObjectError
		if errors.As(err, &invalidErr) {
			return err
		}
		return invalidDeploymentObjectError{err: fmt.Errorf("verify deployment object %s: %w", object.Digest, err)}
	}
	return nil
}

type deploymentObjectReader struct {
	source io.Reader
	err    error
}

func (r *deploymentObjectReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = errors.Join(r.err, err)
	}
	return count, err
}

func verifyStoredProgram(ctx context.Context, source io.Reader, program deployment.ProgramOutput) error {
	file, err := os.CreateTemp("", "helmr-program-verification-*")
	if err != nil {
		return err
	}
	name := file.Name()
	written, copyErr := io.Copy(file, io.LimitReader(source, program.Artifact.SizeBytes+1))
	var verifyErr error
	if copyErr == nil && written == program.Artifact.SizeBytes {
		verifyErr = deployment.VerifyProgramOutputFile(ctx, file, program)
	}
	cleanupErr := errors.Join(file.Close(), os.Remove(name))
	if copyErr != nil || cleanupErr != nil {
		return errors.Join(copyErr, cleanupErr)
	}
	if written != program.Artifact.SizeBytes {
		return deploymentObjectSourceError{err: errors.New("stored Program bytes ended before the admitted descriptor size")}
	}
	if verifyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return invalidDeploymentObjectError{err: verifyErr}
	}
	return nil
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
	objects := make([]cas.Descriptor, 0, len(prepared.objects)+1)
	objects = append(objects, prepared.root)
	objects = append(objects, prepared.objects...)
	for _, object := range objects {
		if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
			OrgID: orgID, Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}); err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment CAS ownership: %w", err)
		}
	}
	artifacts := make(map[string]db.Artifact, len(prepared.objects))
	for _, descriptor := range prepared.objects {
		kind := db.ArtifactKindWorkspaceImage
		if descriptor.MediaType == deployment.ProgramArtifactMediaType {
			kind = db.ArtifactKindDeploymentProgram
		}
		artifact, err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.NewV7()), OrgID: orgID, ProjectID: projectID,
			EnvironmentID: environmentID, Digest: descriptor.Digest, Kind: kind,
			SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType,
		})
		if err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment artifact: %w", err)
		}
		artifacts[descriptor.Digest] = artifact
	}
	programArtifact := artifacts[prepared.bundle.Program.Artifact.Digest]
	deploymentID := uuid.NewV7()
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
			ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: environmentID,
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
