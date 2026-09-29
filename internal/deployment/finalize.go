package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// objectVerificationTimeout bounds the verification of one bundle object.
const objectVerificationTimeout = 10 * time.Minute

// Finalizer finalizes uploaded deployment bundles into Deployments. It reads
// bundle roots and objects from the upload store, publishes quarantined
// objects the organization does not own yet, checks the supported Runtime in
// the platform store and verifies one object at a time.
type Finalizer struct {
	uploads       cas.UploadStore
	platform      cas.Reader
	admission     bundle.Admission
	log           *slog.Logger
	verifierSlots chan struct{}
}

// NewFinalizer returns a Finalizer over the upload and platform stores that
// admits bundles by admission and logs to log.
func NewFinalizer(uploads cas.UploadStore, platform cas.Reader, admission bundle.Admission, log *slog.Logger) *Finalizer {
	return &Finalizer{
		uploads: uploads, platform: platform, admission: admission, log: log,
		verifierSlots: make(chan struct{}, 1),
	}
}

// Finalization is an authorized request to finalize one admitted bundle into a
// Deployment of an environment. Only Prepare creates it.
type Finalization struct {
	orgID         uuid.UUID
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	bundle        preparedBundle
	request       idempotency.Request
}

// BundleDigest is the digest of the bundle root being finalized.
func (f Finalization) BundleDigest() string {
	return f.bundle.root.Digest
}

type preparedBundle struct {
	root        cas.Descriptor
	bundle      bundle.Manifest
	objects     []cas.Descriptor
	definitions []preparedDefinition
	queueConfig []byte
	indexDigest []byte
}

type preparedDefinition struct {
	kind           string
	declaredID     string
	manifest       []byte
	manifestDigest []byte
	computerSpec   *definition.ComputerSpec
}

type finalizeReceipt struct {
	DeploymentID string `json:"deploymentId"`
}

// Prepare authorizes the principal to deploy into the environment, admits the
// uploaded bundle root and binds the idempotency key. It reads the root but no
// bundle object.
func (f *Finalizer) Prepare(ctx context.Context, principal auth.Actor, scope auth.Scope, bundleDigest string, idempotencyKey string) (Finalization, error) {
	if err := authorizeDeploy(principal, scope); err != nil {
		return Finalization{}, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return Finalization{}, err
	}
	prepared, err := f.prepareBundle(ctx, bundleDigest)
	if err != nil {
		return Finalization{}, invalidInput(err)
	}
	request, err := idempotency.NewDeploymentFinalizeRequest(
		pgvalue.MustUUIDValue(environmentID), pgvalue.MustUUIDValue(projectID), idempotencyKey,
		idempotency.DeploymentFinalizeFingerprint{BundleDigest: bundleDigest},
	)
	if err != nil {
		return Finalization{}, invalidInput(err)
	}
	return Finalization{
		orgID: principal.OrgID, projectID: projectID, environmentID: environmentID,
		bundle: prepared, request: request,
	}, nil
}

// Finalize verifies the bundle objects and registers the Deployment, reporting
// each verified object digest to progress. A bundle already finalized in the
// environment whose objects are all still stored is registered again without
// reading them. Registration is one transaction: a replayed idempotency key
// returns its recorded Deployment, and a new key either reuses the
// environment's Deployment of the same bundle or creates it with its artifacts,
// Computer specs and definitions.
func (f *Finalizer) Finalize(ctx context.Context, q db.Querier, txb db.TxBeginner, finalization Finalization, progress func(objectDigest string) error) (db.Deployment, error) {
	replay, err := f.available(ctx, q, finalization.environmentID, finalization.bundle)
	if err != nil {
		return db.Deployment{}, err
	}
	if !replay {
		if err := f.verifyObjects(ctx, q, finalization.orgID, finalization.bundle, progress); err != nil {
			return db.Deployment{}, err
		}
	}
	return register(ctx, txb, finalization)
}

func (f *Finalizer) available(ctx context.Context, q db.Querier, environmentID pgtype.UUID, prepared preparedBundle) (bool, error) {
	_, err := q.GetDeploymentByBundleDigest(ctx, db.GetDeploymentByBundleDigestParams{
		EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve deployment bundle replay: %w", err)
	}
	for _, descriptor := range prepared.objects {
		object, err := f.uploads.Stat(ctx, descriptor.Digest)
		if err != nil || cas.RequireExact(object, descriptor) != nil {
			return false, nil
		}
	}
	return true, nil
}

func register(ctx context.Context, txb db.TxBeginner, finalization Finalization) (db.Deployment, error) {
	orgID := pgvalue.UUID(finalization.orgID)
	projectID, environmentID := finalization.projectID, finalization.environmentID
	prepared := finalization.bundle
	var record db.Deployment
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		claim, err := claims.Acquire(ctx, finalization.request)
		if err != nil {
			return err
		}
		if !claim.New {
			record, err = replayFinalization(ctx, q, claim.Claim, orgID, projectID)
			return err
		}
		if err := q.LockDeploymentBundle(ctx, db.LockDeploymentBundleParams{
			EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
		}); err != nil {
			return fmt.Errorf("lock deployment bundle: %w", err)
		}
		existing, err := q.GetDeploymentByBundleDigest(ctx, db.GetDeploymentByBundleDigestParams{
			EnvironmentID: environmentID, BundleDigest: prepared.root.Digest,
		})
		if err == nil {
			record = existing
			return completeFinalization(ctx, claims, claim.Claim, record.ID)
		}
		if !isNoRows(err) {
			return fmt.Errorf("resolve deployment bundle: %w", err)
		}
		created, err := createDeployment(ctx, q, orgID, projectID, environmentID, prepared)
		if err != nil {
			return err
		}
		record = created
		return completeFinalization(ctx, claims, claim.Claim, record.ID)
	})
	if err != nil {
		return db.Deployment{}, err
	}
	return record, nil
}

type objectSourceError struct{ err error }

func (e objectSourceError) Error() string { return e.err.Error() }
func (e objectSourceError) Unwrap() error { return e.err }

func (f *Finalizer) prepareBundle(ctx context.Context, bundleDigest string) (preparedBundle, error) {
	rootObject, err := f.uploads.Stat(ctx, bundleDigest)
	if err != nil {
		return preparedBundle{}, fmt.Errorf("resolve deployment bundle root: %w", err)
	}
	if rootObject.MediaType != bundle.MediaType ||
		rootObject.SizeBytes < 1 || rootObject.SizeBytes > bundle.MaxBytes {
		return preparedBundle{}, errors.New("deployment bundle root descriptor is invalid")
	}
	rootReader, err := f.uploads.Get(ctx, bundleDigest)
	if err != nil {
		return preparedBundle{}, fmt.Errorf("read deployment bundle root: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(rootReader, bundle.MaxBytes+1))
	closeErr := rootReader.Close()
	if readErr != nil || closeErr != nil {
		return preparedBundle{}, errors.Join(readErr, closeErr)
	}
	actualDigest, err := bundle.Digest(raw)
	if err != nil || actualDigest != bundleDigest || int64(len(raw)) != rootObject.SizeBytes {
		return preparedBundle{}, errors.New("deployment bundle root bytes do not match the request")
	}
	manifest, err := bundle.Parse(raw)
	if err != nil {
		return preparedBundle{}, err
	}
	if err := f.admission.Admit(manifest); err != nil {
		return preparedBundle{}, err
	}
	if err := requireSupportedRuntime(ctx, f.platform, manifest.Runtime.Artifact); err != nil {
		return preparedBundle{}, err
	}
	objects := make([]cas.Descriptor, 0, len(manifest.Objects))
	for _, object := range manifest.Objects {
		objects = append(objects, cas.Descriptor{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		})
	}
	definitions, err := prepareDefinitions(manifest)
	if err != nil {
		return preparedBundle{}, err
	}
	queueConfig, err := canonicalQueueConfig(manifest.Plan)
	if err != nil {
		return preparedBundle{}, err
	}
	index, err := artifact.CanonicalProgramIndex(manifest.Program.Index)
	if err != nil {
		return preparedBundle{}, err
	}
	indexDigest := sha256.Sum256(index)
	return preparedBundle{
		root:   cas.Descriptor{Digest: bundleDigest, SizeBytes: rootObject.SizeBytes, MediaType: rootObject.MediaType},
		bundle: manifest, objects: objects, definitions: definitions,
		queueConfig: queueConfig, indexDigest: indexDigest[:],
	}, nil
}

func requireSupportedRuntime(ctx context.Context, store cas.Reader, runtime bundle.Object) error {
	object, err := store.Stat(ctx, runtime.Digest)
	if err != nil {
		return fmt.Errorf("resolve supported Runtime object: %w", err)
	}
	if err := cas.RequireExact(object, cas.Descriptor{
		Digest: runtime.Digest, SizeBytes: runtime.SizeBytes, MediaType: runtime.MediaType,
	}); err != nil {
		return fmt.Errorf("supported Runtime object: %w", err)
	}
	return nil
}

func (f *Finalizer) verifyObjects(ctx context.Context, q db.Querier, orgID uuid.UUID, prepared preparedBundle, progress func(string) error) error {
	startedAt := time.Now()
	var verifiedBytes int64
	owner := strings.ToLower(orgID.String())
	for _, object := range prepared.objects {
		if err := f.requireObject(ctx, q, owner, orgID, object); err != nil {
			return err
		}
		if err := f.verifyObject(ctx, prepared.bundle, object); err != nil {
			return err
		}
		if err := progress(object.Digest); err != nil {
			return err
		}
		verifiedBytes += object.SizeBytes
	}
	f.log.Info("deployment objects verified",
		"bundle_digest", prepared.root.Digest,
		"object_count", len(prepared.objects),
		"verified_bytes", verifiedBytes,
		"duration", time.Since(startedAt),
	)
	return nil
}

// requireObject confirms that an object the organization owns is stored as the
// bundle describes it, and publishes an object it does not own yet from the
// organization's quarantine.
func (f *Finalizer) requireObject(ctx context.Context, q db.Querier, owner string, orgID uuid.UUID, descriptor cas.Descriptor) error {
	owned, ownershipErr := q.GetCasObject(ctx, db.GetCasObjectParams{
		OrgID: pgvalue.UUID(orgID), Digest: descriptor.Digest,
	})
	switch {
	case ownershipErr == nil:
		if owned.SizeBytes != descriptor.SizeBytes || owned.MediaType != descriptor.MediaType {
			return InvalidObjectError{Err: errors.New("owned deployment object descriptor conflicts with bundle")}
		}
		stored, statErr := f.uploads.Stat(ctx, descriptor.Digest)
		if statErr != nil {
			return fmt.Errorf("owned deployment object is unavailable: %w", statErr)
		}
		if err := cas.RequireExact(stored, descriptor); err != nil {
			return fmt.Errorf("owned deployment object is unavailable: %w", err)
		}
	case isNoRows(ownershipErr):
		stored, promoteErr := f.uploads.PromoteQuarantine(ctx, owner, descriptor)
		if promoteErr != nil {
			return fmt.Errorf("publish deployment object %s: %w", descriptor.Digest, promoteErr)
		}
		if err := cas.RequireExact(stored, descriptor); err != nil {
			return err
		}
	default:
		return fmt.Errorf("resolve deployment object ownership: %w", ownershipErr)
	}
	return nil
}

func (f *Finalizer) verifyObject(ctx context.Context, manifest bundle.Manifest, object cas.Descriptor) error {
	ctx, cancel := context.WithTimeout(ctx, objectVerificationTimeout)
	defer cancel()
	select {
	case f.verifierSlots <- struct{}{}:
		defer func() { <-f.verifierSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	reader, err := f.uploads.Get(ctx, object.Digest)
	if err != nil {
		return fmt.Errorf("read deployment object %s: %w", object.Digest, err)
	}
	recorded := &objectReader{source: reader}
	switch object.MediaType {
	case artifact.ProgramArtifactMediaType:
		err = verifyStoredProgram(ctx, recorded, manifest.Program)
	case bundle.ComputerImageMediaType:
		err = disk.VerifySeed(ctx, recorded, disk.SeedArtifact{Object: object, LogicalBytes: disk.SeedCapacity}, disk.SeedCapacity)
	default:
		err = errors.New("deployment object media type is unsupported")
	}
	closeErr := reader.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if recorded.err != nil || closeErr != nil {
		return fmt.Errorf("read deployment object %s: %w", object.Digest, errors.Join(recorded.err, closeErr))
	}
	if err != nil {
		var sourceErr objectSourceError
		if errors.As(err, &sourceErr) {
			return fmt.Errorf("read deployment object %s: %w", object.Digest, err)
		}
		var invalidErr InvalidObjectError
		if errors.As(err, &invalidErr) {
			return err
		}
		return InvalidObjectError{Err: fmt.Errorf("verify deployment object %s: %w", object.Digest, err)}
	}
	return nil
}

// objectReader records read failures of the stored object, so they are not
// mistaken for invalid object bytes.
type objectReader struct {
	source io.Reader
	err    error
}

func (r *objectReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = errors.Join(r.err, err)
	}
	return count, err
}

func verifyStoredProgram(ctx context.Context, source io.Reader, program artifact.ProgramOutput) error {
	file, err := os.CreateTemp("", "helmr-program-verification-*")
	if err != nil {
		return err
	}
	name := file.Name()
	written, copyErr := io.Copy(file, io.LimitReader(source, program.Artifact.SizeBytes+1))
	var verifyErr error
	if copyErr == nil && written == program.Artifact.SizeBytes {
		verifyErr = verify.ProgramOutputFile(ctx, file, program)
	}
	cleanupErr := errors.Join(file.Close(), os.Remove(name))
	if copyErr != nil || cleanupErr != nil {
		return errors.Join(copyErr, cleanupErr)
	}
	if written != program.Artifact.SizeBytes {
		return objectSourceError{err: errors.New("stored Program bytes ended before the admitted descriptor size")}
	}
	if verifyErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return InvalidObjectError{Err: verifyErr}
	}
	return nil
}

func prepareDefinitions(manifest bundle.Manifest) ([]preparedDefinition, error) {
	images := make(map[string]bundle.ComputerImageArtifact, len(manifest.ComputerImages))
	for _, image := range manifest.ComputerImages {
		images[image.DeclaredID] = image.Artifact
	}
	definitions := make([]preparedDefinition, 0, len(manifest.Plan.Definitions))
	for _, declaration := range manifest.Plan.Definitions {
		var declared any
		var computerSpec *definition.ComputerSpec
		switch declaration.Kind {
		case definition.KindTask:
			declared = declaration.Task
		case definition.KindActor:
			declared = declaration.Actor
		case definition.KindSandbox:
			declared = declaration.Sandbox
			value, ok := images[declaration.DeclaredID]
			if !ok {
				return nil, fmt.Errorf("deployment sandbox %q has no image", declaration.DeclaredID)
			}
			spec, err := definition.CompileComputerSpec(*declaration.Sandbox, value.ComputerImage())
			if err != nil {
				return nil, fmt.Errorf("deployment sandbox %q: %w", declaration.DeclaredID, err)
			}
			computerSpec = &spec
		default:
			return nil, fmt.Errorf("deployment definition kind %q is unsupported", declaration.Kind)
		}
		raw, err := json.Marshal(declared)
		if err != nil {
			return nil, err
		}
		canonical, digest, err := definition.CanonicalManifestAndDigest(raw)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, preparedDefinition{
			kind: string(declaration.Kind), declaredID: declaration.DeclaredID,
			manifest: canonical, manifestDigest: digest[:], computerSpec: computerSpec,
		})
	}
	return definitions, nil
}

func canonicalQueueConfig(plan bundle.Plan) ([]byte, error) {
	queues := make([]definition.QueueInput, len(plan.Queues))
	for index, queue := range plan.Queues {
		queues[index] = queue
		if queue.ConcurrencyLimit != nil {
			value := *queue.ConcurrencyLimit
			queues[index].ConcurrencyLimit = &value
		}
	}
	return definition.CanonicalQueueConfig(definition.QueueConfig{
		FormatVersion: definition.DeploymentPlanFormatVersion, Queues: queues,
	})
}

func createDeployment(
	ctx context.Context,
	q db.Querier,
	orgID, projectID, environmentID pgtype.UUID,
	prepared preparedBundle,
) (db.Deployment, error) {
	objects := make([]cas.Descriptor, 0, len(prepared.objects)+1)
	objects = append(objects, prepared.root)
	objects = append(objects, prepared.objects...)
	for _, object := range objects {
		if _, err := q.UpsertCasObject(ctx, db.UpsertCasObjectParams{
			OrgID: orgID, Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}); err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment CAS ownership: %w", err)
		}
	}
	artifacts := make(map[string]db.Artifact, len(prepared.objects))
	for _, descriptor := range prepared.objects {
		kind := db.ArtifactKindComputerImage
		if descriptor.MediaType == artifact.ProgramArtifactMediaType {
			kind = db.ArtifactKindDeploymentProgram
		}
		row, err := q.CreateArtifact(ctx, db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.NewV7()), OrgID: orgID, ProjectID: projectID,
			EnvironmentID: environmentID, Digest: descriptor.Digest, Kind: kind,
			SizeBytes: descriptor.SizeBytes, MediaType: descriptor.MediaType,
		})
		if err != nil {
			return db.Deployment{}, fmt.Errorf("register deployment artifact: %w", err)
		}
		artifacts[descriptor.Digest] = row
	}
	programArtifact := artifacts[prepared.bundle.Program.Artifact.Digest]
	deploymentID := uuid.NewV7()
	record, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: pgvalue.UUID(deploymentID), OrgID: orgID, ProjectID: projectID,
		EnvironmentID: environmentID, Version: version(deploymentID),
		BundleDigest:          prepared.root.Digest,
		RuntimeArtifactDigest: prepared.bundle.Runtime.Artifact.Digest,
		ProgramArtifactID:     programArtifact.ID, ProgramIndexDigest: prepared.indexDigest,
		QueueConfig: prepared.queueConfig,
	})
	if err != nil {
		return db.Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	specs, err := registerComputerSpecs(ctx, q, environmentID, prepared.definitions, artifacts)
	if err != nil {
		return db.Deployment{}, err
	}
	if err := createDefinitions(ctx, q, environmentID, record.ID, prepared.definitions, specs); err != nil {
		return db.Deployment{}, err
	}
	for _, registered := range artifacts {
		if registered.Kind != db.ArtifactKindComputerImage {
			continue
		}
		if err := q.DeleteUnusedComputerSeedArtifact(ctx, db.DeleteUnusedComputerSeedArtifactParams{EnvironmentID: environmentID, ID: registered.ID}); err != nil {
			return db.Deployment{}, fmt.Errorf("release redundant computer seed artifact: %w", err)
		}
	}
	return record, nil
}

// registerComputerSpecs registers the immutable Computer specs of the bundle's
// Sandboxes in the transaction that records the definitions referencing them.
// Reuse never changes launch content.
func registerComputerSpecs(
	ctx context.Context,
	q db.Querier,
	environmentID pgtype.UUID,
	definitions []preparedDefinition,
	artifacts map[string]db.Artifact,
) (map[string]db.ComputerSpec, error) {
	specs := make(map[string]db.ComputerSpec)
	ordered := make([]preparedDefinition, 0, len(definitions))
	for _, prepared := range definitions {
		if prepared.computerSpec != nil {
			ordered = append(ordered, prepared)
		}
	}
	// Deployments can name the same specs in different orders. Acquire their
	// unique-key locks in content order to avoid a cross-deployment deadlock.
	slices.SortFunc(ordered, func(left, right preparedDefinition) int {
		return bytes.Compare(left.computerSpec.Digest[:], right.computerSpec.Digest[:])
	})
	for _, prepared := range ordered {
		spec := prepared.computerSpec
		seed, ok := artifacts[spec.Seed.Digest]
		if !ok {
			return nil, fmt.Errorf("computer seed %q is not registered", spec.Seed.Digest)
		}
		stored, err := q.RegisterComputerSpec(ctx, db.RegisterComputerSpecParams{
			ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: environmentID,
			Config: spec.Config, Digest: spec.Digest[:], SeedArtifactID: seed.ID,
			SeedDigest: spec.Seed.Digest, SeedSizeBytes: spec.Seed.SizeBytes, SeedMediaType: spec.Seed.MediaType,
		})
		if err != nil {
			return nil, fmt.Errorf("register computer spec: %w", err)
		}
		roundTrip, err := definition.ParseComputerSpec(stored.Config, cas.Descriptor{
			Digest: stored.SeedDigest, SizeBytes: stored.SeedSizeBytes, MediaType: stored.SeedMediaType,
		})
		if err != nil {
			return nil, fmt.Errorf("read registered computer spec: %w", err)
		}
		if roundTrip.Digest != spec.Digest || !bytes.Equal(roundTrip.Digest[:], stored.Digest) ||
			!bytes.Equal(roundTrip.Config, spec.Config) {
			return nil, fmt.Errorf("registered computer spec has different canonical content")
		}
		specs[prepared.declaredID] = stored
	}
	return specs, nil
}

type definitionCreator interface {
	CreateDeploymentDefinitions(context.Context, db.CreateDeploymentDefinitionsParams) (int64, error)
}

// createDefinitions records every definition of the Deployment in one bulk
// statement.
func createDefinitions(
	ctx context.Context,
	q definitionCreator,
	environmentID, deploymentID pgtype.UUID,
	definitions []preparedDefinition,
	specs map[string]db.ComputerSpec,
) error {
	definitionCount := len(definitions)
	params := db.CreateDeploymentDefinitionsParams{
		Ids:             make([]pgtype.UUID, definitionCount),
		Kinds:           make([]string, definitionCount),
		DeclaredIds:     make([]string, definitionCount),
		Manifests:       make([][]byte, definitionCount),
		ManifestDigests: make([][]byte, definitionCount),
		ComputerSpecIds: make([]pgtype.UUID, definitionCount),
		EnvironmentID:   environmentID,
		DeploymentID:    deploymentID,
		ManifestVersion: definition.DeploymentPlanFormatVersion,
	}
	for index, prepared := range definitions {
		params.Ids[index] = pgvalue.UUID(uuid.NewV7())
		params.Kinds[index] = prepared.kind
		params.DeclaredIds[index] = prepared.declaredID
		params.Manifests[index] = prepared.manifest
		params.ManifestDigests[index] = prepared.manifestDigest
		if prepared.kind != string(definition.KindSandbox) {
			continue
		}
		spec, ok := specs[prepared.declaredID]
		if !ok {
			return fmt.Errorf(
				"create deployment definition: computer spec for %q is not registered",
				prepared.declaredID,
			)
		}
		params.ComputerSpecIds[index] = spec.ID
	}
	inserted, err := q.CreateDeploymentDefinitions(ctx, params)
	if err != nil {
		return fmt.Errorf("create deployment definition: %w", err)
	}
	if inserted != int64(definitionCount) {
		return fmt.Errorf(
			"create deployment definition: inserted %d of %d rows",
			inserted,
			definitionCount,
		)
	}
	return nil
}

func completeFinalization(ctx context.Context, claims *idempotency.Transaction, claim db.IdempotencyClaim, deploymentID pgtype.UUID) error {
	receipt, err := json.Marshal(finalizeReceipt{DeploymentID: pgvalue.UUIDString(deploymentID)})
	if err != nil {
		return err
	}
	_, err = claims.Complete(ctx, claim, receipt)
	return err
}

// replayFinalization returns the Deployment a completed finalization recorded
// in its receipt.
func replayFinalization(ctx context.Context, q db.Querier, claim db.IdempotencyClaim, orgID pgtype.UUID, projectID pgtype.UUID) (db.Deployment, error) {
	if claim.Status != "completed" {
		return db.Deployment{}, ErrFinalizationInProgress
	}
	var receipt finalizeReceipt
	decoder := json.NewDecoder(bytes.NewReader(claim.Receipt))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || receipt.DeploymentID == "" {
		return db.Deployment{}, errors.New("deployment finalization receipt is invalid")
	}
	id, err := uuid.Parse(receipt.DeploymentID)
	if err != nil {
		return db.Deployment{}, errors.New("deployment finalization receipt is invalid")
	}
	return q.GetDeployment(ctx, db.GetDeploymentParams{
		OrgID: orgID, ProjectID: projectID,
		EnvironmentID: claim.EnvironmentID, ID: pgvalue.UUID(id),
	})
}
