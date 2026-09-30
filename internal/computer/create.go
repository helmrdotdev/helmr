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
	ID             pgtype.UUID
	Key            pgtype.Text
	Status         string
	LastActivityAt pgtype.Timestamptz
	CreatedAt      pgtype.Timestamptz
	UpdatedAt      pgtype.Timestamptz
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
		ID: created.ID, Key: created.Key, Status: created.Status,
		LastActivityAt: created.LastActivityAt, CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt,
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
		ID: created.ID, Key: created.Key, Status: created.Status,
		LastActivityAt: created.LastActivityAt, CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt,
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

	computerID := uuid.NewV7()
	versionID := uuid.NewV7()
	key := pgtype.Text{}
	if request.Key != nil {
		key = pgtype.Text{String: *request.Key, Valid: true}
	}
	created, err := plan.source.insert(ctx, q, request, sandbox, computerID, versionID, key)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) &&
			postgresError.ConstraintName == "computers_environment_key_uidx" &&
			request.Key != nil {
			return Created{}, KeyConflictError{Key: *request.Key}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return Created{}, ErrNotDeployed
		}
		return Created{}, fmt.Errorf("create computer: %w", err)
	}
	for _, placement := range plan.placements {
		if placement.Mode != "protected" {
			continue
		}
		// The CA expiry is anchored to the inserted row's CreatedAt and is
		// persisted in the inserting transaction, never repaired later.
		trust, err := c.ca.GenerateProxyTrust(request.Scope.EnvironmentID, computerID, created.CreatedAt.Time)
		if err != nil {
			return Created{}, err
		}
		count, err := q.InitializeComputerSecretCA(ctx, db.InitializeComputerSecretCAParams{
			EnvironmentID: pgvalue.UUID(trust.EnvironmentID), ComputerID: pgvalue.UUID(trust.ComputerID),
			Certificate: trust.Certificate, PrivateKeyNonce: trust.PrivateKeyNonce,
			PrivateKeyCiphertext: trust.PrivateKeyCiphertext, NotAfter: pgvalue.Timestamptz(trust.NotAfter),
		})
		if err != nil {
			return Created{}, err
		}
		if count != 1 {
			return Created{}, ErrSecretUnavailable
		}
		break
	}
	for _, placement := range plan.placements {
		placeholder, err := secretbinding.Placeholder(placement.Mode)
		if err != nil {
			return Created{}, err
		}
		if _, err := q.CreateComputerSecret(ctx, db.CreateComputerSecretParams{
			ComputerID:      created.ID,
			EnvironmentID:   pgvalue.UUID(request.Scope.EnvironmentID),
			PlacementKind:   placement.Kind,
			PlacementTarget: placement.Target,
			SecretID:        secretIDs[placement.Name],
			Mode:            placement.Mode, AllowedOrigins: placement.AllowedOrigins, Placeholder: placeholder,
		}); err != nil {
			return Created{}, fmt.Errorf("create computer secret placement: %w", err)
		}
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
