package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
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
	retryKey      string
}

// BundleDigest is the digest of the bundle root being finalized.
func (f Finalization) BundleDigest() string {
	return f.bundle.root.Digest
}

type preparedBundle struct {
	root    cas.Descriptor
	bundle  bundle.Manifest
	objects []cas.Descriptor
}

// Record is a finalized Deployment scoped to its owning Environment.
type Record struct {
	ID            uuid.UUID
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	BundleDigest  string
	CreatedAt     time.Time
}

// Prepare authorizes the principal to deploy into the environment, admits the
// uploaded bundle root and binds the idempotency key. It reads the root but no
// bundle object.
func (f *Finalizer) Prepare(ctx context.Context, principal auth.Principal, scope auth.Scope, bundleDigest string, idempotencyKey string) (Finalization, error) {
	if err := authorizeDeploy(principal, scope); err != nil {
		return Finalization{}, err
	}
	projectID, environmentID, err := scopeIDs(scope)
	if err != nil {
		return Finalization{}, err
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 512 || !utf8.ValidString(idempotencyKey) || strings.ContainsFunc(idempotencyKey, unicode.IsControl) {
		return Finalization{}, invalidInput(errors.New("finalization retry key must contain 1 to 512 bytes without control characters"))
	}
	prepared, err := f.prepareBundle(ctx, bundleDigest)
	if err != nil {
		return Finalization{}, invalidInput(err)
	}

	return Finalization{
		orgID: principal.OrgID, projectID: projectID, environmentID: environmentID,
		bundle: prepared, retryKey: idempotencyKey,
	}, nil
}

// Finalize verifies all referenced objects before atomically registering the
// Deployment and its definitions. An operation-specific receipt binds a retry
// key to the exact bundle; distinct keys for that bundle converge on one record.
func (f *Finalizer) Finalize(ctx context.Context, database db.TxDB, finalization Finalization, progress func(objectDigest string) error) (Record, error) {
	available, err := f.available(ctx, database, finalization)
	if err != nil {
		return Record{}, err
	}
	if !available {
		if err := f.verifyObjects(ctx, db.New(database), finalization.orgID, finalization.bundle, progress); err != nil {
			return Record{}, err
		}
	}
	return register(ctx, database, finalization)
}

// Only a previously committed registration in this authorized Environment can
// avoid re-reading large objects. Missing or changed objects must verify again.
func (f *Finalizer) available(ctx context.Context, database db.DBTX, request Finalization) (bool, error) {
	var registered bool
	err := database.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE d.environment_id=$1 AND d.bundle_digest=$2 AND e.org_id=$3 AND e.project_id=$4 AND e.retired_at IS NULL)`, request.environmentID, request.bundle.root.Digest, request.orgID, request.projectID).Scan(&registered)
	if err != nil || !registered {
		return false, err
	}
	for _, descriptor := range request.bundle.objects {
		object, err := f.uploads.Stat(ctx, descriptor.Digest)
		if err != nil || cas.RequireExact(object, descriptor) != nil {
			return false, nil
		}
	}
	return true, nil
}

func register(ctx context.Context, txb db.TxBeginner, f Finalization) (Record, error) {
	var record Record
	env, project := pgvalue.MustUUIDValue(f.environmentID), pgvalue.MustUUIDValue(f.projectID)
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		// The Environment serializes finalization, promotion and retirement. Check
		// scope again while locked, before any definitions or retry receipts exist.
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 AND org_id=$2 AND project_id=$3 AND retired_at IS NULL FOR NO KEY UPDATE`, env, f.orgID, project).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		request, err := idempotency.NewDeploymentFinalizeRequest(env, project, f.retryKey, idempotency.DeploymentFinalizeFingerprint{BundleDigest: f.bundle.root.Digest})
		if err != nil {
			return err
		}
		claims, _ := idempotency.TransactionFor(tx)
		acquired, err := claims.Acquire(ctx, request)
		if err != nil {
			var conflict idempotency.ConflictError
			if errors.As(err, &conflict) {
				return errors.Join(ErrFinalizationConflict, err)
			}
			return err
		}
		if !acquired.New {
			if err = json.Unmarshal(acquired.Claim.Receipt, &record); err != nil {
				return err
			}
			if record.ID != pgvalue.MustUUIDValue(acquired.Claim.DeploymentID) || record.EnvironmentID != env || record.ProjectID != project || record.OrgID != f.orgID || record.BundleDigest != f.bundle.root.Digest {
				return idempotency.ErrIncomplete
			}
			return nil
		}
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM deployments WHERE environment_id=$1 AND bundle_digest=$2`, env, f.bundle.root.Digest).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			record, err = createDeployment(ctx, tx, f)
		} else if err == nil {
			record, err = readRecord(ctx, tx, env, existing)
		}
		if err != nil {
			return err
		}
		receipt, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = claims.Complete(ctx, acquired.Claim, idempotency.Target{DeploymentID: record.ID}, receipt)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return record, nil
}

func readRecord(ctx context.Context, q db.DBTX, env, id uuid.UUID) (Record, error) {
	var r Record
	err := q.QueryRow(ctx, `SELECT d.id,e.org_id,e.project_id,d.environment_id,d.bundle_digest,d.created_at FROM deployments d JOIN environments e ON e.id=d.environment_id WHERE d.environment_id=$1 AND d.id=$2`, env, id).Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.EnvironmentID, &r.BundleDigest, &r.CreatedAt)
	return r, err
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
	return preparedBundle{
		root:   cas.Descriptor{Digest: bundleDigest, SizeBytes: rootObject.SizeBytes, MediaType: rootObject.MediaType},
		bundle: manifest, objects: objects,
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
	case definition.ComputerSeedMediaType:
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

func createDeployment(ctx context.Context, tx pgx.Tx, f Finalization) (Record, error) {
	q := db.New(tx)
	objects := append([]cas.Descriptor{f.bundle.root}, f.bundle.objects...)
	for _, object := range objects {
		if _, err := q.UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.orgID), Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}); err != nil {
			return Record{}, fmt.Errorf("register deployment CAS ownership: %w", err)
		}
	}
	env := pgvalue.MustUUIDValue(f.environmentID)
	id := uuid.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3)`, env, id, f.bundle.root.Digest); err != nil {
		return Record{}, err
	}
	for _, object := range objects {
		if _, err := tx.Exec(ctx, `INSERT INTO deployment_objects(environment_id,deployment_id,org_id,project_id,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,$6,$7)`, env, id, f.orgID, f.projectID, object.Digest, object.SizeBytes, object.MediaType); err != nil {
			return Record{}, fmt.Errorf("retain deployment dependency: %w", err)
		}
	}
	if err := agent.RegisterProgramDefinitions(ctx, tx, env, id, f.bundle.bundle.Program); err != nil {
		return Record{}, err
	}
	if _, err := q.AppendDeploymentEvent(ctx, db.AppendDeploymentEventParams{EnvironmentID: pgvalue.UUID(env), DeploymentID: pgvalue.UUID(id), OrgID: pgvalue.UUID(f.orgID), ProjectID: f.projectID, Kind: "deployment.registered", Message: "Deployment registered", Payload: []byte(`{}`)}); err != nil {
		return Record{}, err
	}
	return readRecord(ctx, tx, env, id)
}
