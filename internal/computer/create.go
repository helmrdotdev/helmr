package computer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// CAIssuer generates the secret proxy CA of a Computer that has protected
// bindings. It neither persists nor retrieves material: creation persists
// the CA in the transaction that inserted the Computer, anchored to that
// row's CreatedAt. secret.Store implements it.
type CAIssuer interface {
	GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error)
}

// Creator creates Computers. Its CA issuer must be non-nil.
type Creator struct {
	ca CAIssuer
}

func NewCreator(ca CAIssuer) Creator {
	return Creator{ca: ca}
}

// Request is a Computer creation in one environment.
type Request struct {
	Scope          Scope
	DeclaredID     string
	Key            *string
	Secrets        []secretbinding.Binding
	IdempotencyKey string
}

// Created is a created or replayed Computer.
type Created struct {
	ComputerID uuid.UUID
	Snapshot   Snapshot
	Replayed   bool
}

// Create creates a Computer from the environment's current deployment. In
// one transaction it acquires the idempotency claim, resolves the Sandbox
// declaration, locks the bound Secrets, inserts the Computer, persists its
// CA when a binding is protected, writes the placements and completes the
// claim with the snapshot as receipt.
func (c Creator) Create(ctx context.Context, txb db.TxBeginner, request Request) (Created, error) {
	plan, err := newCreation(request, currentDeployment{}, func(fingerprint idempotency.ComputerCreateFingerprint) (idempotency.Request, error) {
		return idempotency.NewExternalComputerCreateRequest(
			request.Scope.EnvironmentID, request.DeclaredID, request.IdempotencyKey, fingerprint,
		)
	})
	if err != nil {
		return Created{}, err
	}
	var created Created
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		created, err = c.create(ctx, tx, plan)
		return err
	})
	return created, err
}

// RunCreation is a validated Computer creation from a Run's pinned
// deployment, by a live source Run.
type RunCreation struct {
	creator Creator
	plan    creation
}

// PrepareRunCreation validates a creation from the deployment Run runID is
// pinned to. sourceComputerID is the source Run's Computer, whose secret
// authority bounds a replayed Computer.
func (c Creator) PrepareRunCreation(request Request, runID, sourceComputerID uuid.UUID) (RunCreation, error) {
	plan, err := newCreation(request, runDeployment{runID: runID}, func(fingerprint idempotency.ComputerCreateFingerprint) (idempotency.Request, error) {
		return idempotency.NewRuntimeComputerCreateRequest(
			request.Scope.EnvironmentID, runID, request.DeclaredID, request.IdempotencyKey, fingerprint,
		)
	})
	if err != nil {
		return RunCreation{}, err
	}
	plan.replayCeiling = pgvalue.UUID(sourceComputerID)
	return RunCreation{creator: c, plan: plan}, nil
}

// Create runs the creation in the caller's transaction. The caller must
// already hold, in order, the requested Secrets under the source ceiling
// (LockSecretsWithinCeiling) and the live source Run authority, including
// for replays; creation then acquires the idempotency claim. A replay also
// checks the replayed Computer against the source ceiling.
func (r RunCreation) Create(ctx context.Context, tx pgx.Tx) (Created, error) {
	return r.creator.create(ctx, tx, r.plan)
}

type creation struct {
	request       Request
	placements    []secretbinding.Placement
	claim         idempotency.Request
	source        deploymentSource
	replayCeiling pgtype.UUID
}

func newCreation(
	request Request,
	source deploymentSource,
	claimRequest func(idempotency.ComputerCreateFingerprint) (idempotency.Request, error),
) (creation, error) {
	placements, err := secretbinding.NormalizedPlacements(request.Secrets)
	if err != nil {
		return creation{}, invalidCreate(err)
	}
	if err := ValidateKey(request.Key); err != nil {
		return creation{}, invalidCreate(err)
	}
	placementJSON, err := json.Marshal(placements)
	if err != nil {
		return creation{}, fmt.Errorf("encode computer secret placements: %w", err)
	}
	plan := creation{request: request, placements: placements, source: source}
	if request.IdempotencyKey != "" {
		plan.claim, err = claimRequest(idempotency.ComputerCreateFingerprint{
			Key: request.Key, Secrets: placementJSON,
		})
		if err != nil {
			return creation{}, invalidCreate(err)
		}
	}
	return plan, nil
}

func invalidCreate(err error) error {
	return invalidInput("computer create request is invalid: %v", err)
}

// deploymentSource selects the deployment a creation resolves the Sandbox
// declaration from and inserts the Computer with.
type deploymentSource interface {
	resolve(ctx context.Context, q db.Querier, request Request) (db.DeploymentDefinition, error)
	insert(ctx context.Context, q db.Querier, request Request, definition db.DeploymentDefinition, computerID, versionID uuid.UUID, key pgtype.Text) (insertedComputer, error)
}

type insertedComputer struct {
	ID                pgtype.UUID
	Key               pgtype.Text
	Status            string
	Revision          int64
	HeadDiskVersionID pgtype.UUID
	LastActivityAt    pgtype.Timestamptz
	CreatedAt         pgtype.Timestamptz
	UpdatedAt         pgtype.Timestamptz
}

type currentDeployment struct{}

func (currentDeployment) resolve(ctx context.Context, q db.Querier, request Request) (db.DeploymentDefinition, error) {
	return q.ResolveCurrentComputerDefinitionForCreate(ctx, db.ResolveCurrentComputerDefinitionForCreateParams{
		EnvironmentID:     pgvalue.UUID(request.Scope.EnvironmentID),
		SandboxDeclaredID: request.DeclaredID,
	})
}

func (currentDeployment) insert(ctx context.Context, q db.Querier, request Request, definition db.DeploymentDefinition, computerID, versionID uuid.UUID, key pgtype.Text) (insertedComputer, error) {
	created, err := q.CreateComputerFromCurrentDeployment(ctx, db.CreateComputerFromCurrentDeploymentParams{
		ProjectID:              pgvalue.UUID(request.Scope.ProjectID),
		OrgID:                  pgvalue.UUID(request.Scope.OrgID),
		EnvironmentID:          pgvalue.UUID(request.Scope.EnvironmentID),
		DeploymentDefinitionID: definition.ID,
		SandboxDeclaredID:      request.DeclaredID,
		ID:                     pgvalue.UUID(computerID),
		InitialVersionID:       pgvalue.UUID(versionID),
		Key:                    key,
	})
	return insertedComputer{
		ID: created.ID, Key: created.Key, Status: created.Status, Revision: created.Revision,
		HeadDiskVersionID: created.HeadDiskVersionID, LastActivityAt: created.LastActivityAt,
		CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt,
	}, err
}

type runDeployment struct {
	runID uuid.UUID
}

func (s runDeployment) resolve(ctx context.Context, q db.Querier, request Request) (db.DeploymentDefinition, error) {
	return q.ResolveRunPinnedComputerDefinitionForCreate(ctx, db.ResolveRunPinnedComputerDefinitionForCreateParams{
		EnvironmentID:     pgvalue.UUID(request.Scope.EnvironmentID),
		RunID:             pgvalue.UUID(s.runID),
		SandboxDeclaredID: request.DeclaredID,
	})
}

func (s runDeployment) insert(ctx context.Context, q db.Querier, request Request, _ db.DeploymentDefinition, computerID, versionID uuid.UUID, key pgtype.Text) (insertedComputer, error) {
	created, err := q.CreateComputerFromRunDeployment(ctx, db.CreateComputerFromRunDeploymentParams{
		EnvironmentID:     pgvalue.UUID(request.Scope.EnvironmentID),
		RunID:             pgvalue.UUID(s.runID),
		SandboxDeclaredID: request.DeclaredID,
		ID:                pgvalue.UUID(computerID),
		InitialVersionID:  pgvalue.UUID(versionID),
		Key:               key,
	})
	return insertedComputer{
		ID: created.ID, Key: created.Key, Status: created.Status, Revision: created.Revision,
		HeadDiskVersionID: created.HeadDiskVersionID, LastActivityAt: created.LastActivityAt,
		CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt,
	}, err
}

type createReceipt struct {
	Computer Snapshot `json:"computer"`
}

func (c Creator) create(ctx context.Context, tx pgx.Tx, plan creation) (Created, error) {
	q := db.New(tx)
	request := plan.request
	var claim *db.IdempotencyClaim
	if plan.claim != nil {
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return Created{}, err
		}
		acquired, err := claims.Acquire(ctx, plan.claim)
		if err != nil {
			return Created{}, err
		}
		if acquired.Claim.Status == "completed" {
			replayed, err := createdFromReceipt(acquired.Claim.Receipt)
			if err != nil {
				return Created{}, err
			}
			if plan.replayCeiling.Valid {
				if err := CheckSecretTarget(ctx, q, plan.replayCeiling, pgvalue.UUID(replayed.ComputerID)); err != nil {
					return Created{}, err
				}
			}
			replayed.Replayed = true
			return replayed, nil
		}
		if acquired.Claim.Status != "pending" {
			return Created{}, ErrReceiptInvalid
		}
		claim = &acquired.Claim
	}

	sandbox, err := plan.source.resolve(ctx, q, request)
	if errors.Is(err, pgx.ErrNoRows) {
		return Created{}, ErrNotDeployed
	}
	if err != nil {
		return Created{}, fmt.Errorf("resolve computer declaration: %w", err)
	}

	secretIDs := make(map[string]pgtype.UUID, len(plan.placements))
	secretNames := make([]string, 0, len(plan.placements))
	for _, placement := range plan.placements {
		if _, ok := secretIDs[placement.Name]; !ok {
			secretIDs[placement.Name] = pgtype.UUID{}
			secretNames = append(secretNames, placement.Name)
		}
	}
	sort.Strings(secretNames)
	if len(secretNames) > 0 {
		records, err := q.LockActiveSecretsByNameForComputerCreate(ctx, db.LockActiveSecretsByNameForComputerCreateParams{
			EnvironmentID: pgvalue.UUID(request.Scope.EnvironmentID),
			Names:         secretNames,
		})
		if err != nil {
			return Created{}, fmt.Errorf("lock computer secrets: %w", err)
		}
		for _, record := range records {
			secretIDs[record.Name] = record.ID
		}
		for _, name := range secretNames {
			if !secretIDs[name].Valid {
				return Created{}, fmt.Errorf("%w: %s", ErrSecretUnavailable, name)
			}
		}
	}

	locked := make([]LockedSecret, 0, len(plan.placements))
	for _, placement := range plan.placements {
		locked = append(locked, LockedSecret{
			SecretID: pgvalue.MustUUIDValue(secretIDs[placement.Name]),
			Kind:     placement.Kind, Target: placement.Target,
			Mode: placement.Mode, AllowedOrigins: placement.AllowedOrigins,
		})
	}
	key := pgtype.Text{}
	if request.Key != nil {
		key = pgtype.Text{String: *request.Key, Valid: true}
	}
	computerID, created, err := c.install(ctx, q, request.Scope.EnvironmentID, request.Key, locked, func(computerID, versionID uuid.UUID) (insertedComputer, error) {
		return plan.source.insert(ctx, q, request, sandbox, computerID, versionID, key)
	})
	if err != nil {
		return Created{}, err
	}
	status, err := publicStatus(created.Status)
	if err != nil {
		return Created{}, err
	}
	secrets := make([]secretbinding.Binding, 0, len(plan.placements))
	for _, placement := range plan.placements {
		item, err := snapshotBinding(placement.Name, placement.Kind, placement.Target, placement.Mode, placement.AllowedOrigins)
		if err != nil {
			return Created{}, err
		}
		secrets = append(secrets, item)
	}
	result := Created{ComputerID: computerID, Snapshot: Snapshot{
		Residency:      "cold",
		ID:             computerID.String(),
		Key:            textPointer(created.Key),
		SandboxID:      sandbox.DeclaredID,
		DeploymentID:   pgvalue.UUIDString(sandbox.DeploymentID),
		Status:         status,
		Secrets:        secrets,
		LastActivityAt: pgvalue.Time(created.LastActivityAt),
		CreatedAt:      pgvalue.Time(created.CreatedAt),
		UpdatedAt:      pgvalue.Time(created.UpdatedAt),
	}}
	if claim != nil {
		receipt, err := json.Marshal(createReceipt{Computer: result.Snapshot})
		if err != nil {
			return Created{}, err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return Created{}, err
		}
		if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
			return Created{}, err
		}
	}
	return result, nil
}

// LockedSecret is a Secret placement of a new Computer whose Secret the
// creating transaction already holds locked.
type LockedSecret struct {
	SecretID       uuid.UUID
	Kind           string
	Target         string
	Mode           string
	AllowedOrigins []string
}

// install inserts a Computer and its initial disk version with insert,
// persists its secret proxy CA when a placement is protected and writes its
// Secret placements in order, all in the caller's transaction. The CA is
// generated from the inserted row's CreatedAt. key is the requested
// Computer key, for reporting a key conflict.
func (c Creator) install(
	ctx context.Context,
	q *db.Queries,
	environmentID uuid.UUID,
	key *string,
	secrets []LockedSecret,
	insert func(computerID, versionID uuid.UUID) (insertedComputer, error),
) (uuid.UUID, insertedComputer, error) {
	computerID := uuid.NewV7()
	versionID := uuid.NewV7()
	created, err := insert(computerID, versionID)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) &&
			postgresError.ConstraintName == "computers_environment_key_uidx" &&
			key != nil {
			return uuid.UUID{}, insertedComputer{}, KeyConflictError{Key: *key}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.UUID{}, insertedComputer{}, ErrNotDeployed
		}
		return uuid.UUID{}, insertedComputer{}, fmt.Errorf("create computer: %w", err)
	}
	for _, placement := range secrets {
		if placement.Mode != "protected" {
			continue
		}
		// The CA expiry is anchored to the inserted row's CreatedAt and is
		// persisted in the inserting transaction, never repaired later.
		trust, err := c.ca.GenerateProxyTrust(environmentID, computerID, created.CreatedAt.Time)
		if err != nil {
			return uuid.UUID{}, insertedComputer{}, err
		}
		count, err := q.InitializeComputerSecretCA(ctx, db.InitializeComputerSecretCAParams{
			EnvironmentID: pgvalue.UUID(trust.EnvironmentID), ComputerID: pgvalue.UUID(trust.ComputerID),
			Certificate: trust.Certificate, PrivateKeyNonce: trust.PrivateKeyNonce,
			PrivateKeyCiphertext: trust.PrivateKeyCiphertext, NotAfter: pgvalue.Timestamptz(trust.NotAfter),
		})
		if err != nil {
			return uuid.UUID{}, insertedComputer{}, err
		}
		if count != 1 {
			return uuid.UUID{}, insertedComputer{}, ErrSecretUnavailable
		}
		break
	}
	for _, placement := range secrets {
		placeholder, err := secretbinding.Placeholder(placement.Mode)
		if err != nil {
			return uuid.UUID{}, insertedComputer{}, err
		}
		if _, err := q.CreateComputerSecret(ctx, db.CreateComputerSecretParams{
			ComputerID:      created.ID,
			EnvironmentID:   pgvalue.UUID(environmentID),
			PlacementKind:   placement.Kind,
			PlacementTarget: placement.Target,
			SecretID:        pgvalue.UUID(placement.SecretID),
			Mode:            placement.Mode, AllowedOrigins: placement.AllowedOrigins, Placeholder: placeholder,
		}); err != nil {
			return uuid.UUID{}, insertedComputer{}, fmt.Errorf("create computer secret placement: %w", err)
		}
	}
	return computerID, created, nil
}

// ScheduledRequest is the Computer of one schedule fire, created from the
// Sandbox declaration of the deployment the schedule is pinned to.
type ScheduledRequest struct {
	EnvironmentID      uuid.UUID
	ScheduleID         uuid.UUID
	ScheduleGeneration int64
	SandboxDeclaredID  string
	// Secrets are the schedule's Secret placements, in the order they are
	// written.
	Secrets []LockedSecret
}

// ScheduledComputer is a Computer created for a schedule fire.
type ScheduledComputer struct {
	ID                uuid.UUID
	HeadDiskVersionID uuid.UUID
	Revision          int64
}

// CreateScheduled creates the Computer of a schedule fire in the caller's
// transaction: the Computer and its initial disk version, its secret proxy
// CA when a placement is protected, and its Secret placements. The caller
// must already hold the fire's environment, the active schedule at
// ScheduleGeneration and every placement's Secret, in the fire's lock order.
// ErrNotDeployed reports that the schedule is no longer active at that
// generation or that its pinned deployment has no such Sandbox declaration.
func (c Creator) CreateScheduled(ctx context.Context, tx pgx.Tx, request ScheduledRequest) (ScheduledComputer, error) {
	q := db.New(tx)
	_, created, err := c.install(ctx, q, request.EnvironmentID, nil, request.Secrets, func(computerID, versionID uuid.UUID) (insertedComputer, error) {
		created, err := q.CreateComputerForScheduleFire(ctx, db.CreateComputerForScheduleFireParams{
			SandboxDeclaredID:  request.SandboxDeclaredID,
			EnvironmentID:      pgvalue.UUID(request.EnvironmentID),
			ScheduleID:         pgvalue.UUID(request.ScheduleID),
			ExpectedGeneration: request.ScheduleGeneration,
			ID:                 pgvalue.UUID(computerID),
			InitialVersionID:   pgvalue.UUID(versionID),
		})
		return insertedComputer{
			ID: created.ID, Key: created.Key, Status: created.Status, Revision: created.Revision,
			HeadDiskVersionID: created.HeadDiskVersionID, LastActivityAt: created.LastActivityAt,
			CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt,
		}, err
	})
	if err != nil {
		return ScheduledComputer{}, err
	}
	return ScheduledComputer{
		ID:                pgvalue.MustUUIDValue(created.ID),
		HeadDiskVersionID: pgvalue.MustUUIDValue(created.HeadDiskVersionID),
		Revision:          created.Revision,
	}, nil
}

func createdFromReceipt(raw []byte) (Created, error) {
	var receipt createReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Created{}, ErrReceiptInvalid
	}
	computerID, err := ids.Parse(receipt.Computer.ID)
	if err != nil {
		return Created{}, ErrReceiptInvalid
	}
	if ids.Validate(receipt.Computer.DeploymentID) != nil ||
		definition.ValidateSandboxDeclaredID(receipt.Computer.SandboxID) != nil ||
		receipt.Computer.Status != StatusAvailable ||
		receipt.Computer.LastActivityAt.IsZero() ||
		receipt.Computer.CreatedAt.IsZero() ||
		receipt.Computer.UpdatedAt.IsZero() ||
		ValidateKey(receipt.Computer.Key) != nil {
		return Created{}, ErrReceiptInvalid
	}
	if _, err := secretbinding.NormalizedPlacements(receipt.Computer.Secrets); err != nil {
		return Created{}, ErrReceiptInvalid
	}
	return Created{ComputerID: computerID, Snapshot: receipt.Computer}, nil
}
