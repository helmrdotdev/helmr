package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errComputerCreateInvalid        = errors.New("computer create request is invalid")
	errComputerNotDeployed          = errors.New("computer declaration is not deployed")
	errComputerSecretUnavailable    = errors.New("computer secret is unavailable")
	errComputerCreateReceipt        = errors.New("computer create idempotency receipt is invalid")
	errComputerAuthorityUnavailable = errors.New("computer authority is unavailable")
)

type ComputerKeyConflictError struct {
	Key string
}

func (e ComputerKeyConflictError) Error() string {
	return fmt.Sprintf("computer key %q is already in use", e.Key)
}

type computerCreateRequest struct {
	SourceComputerID pgtype.UUID
	OrgID            uuid.UUID
	ProjectID        uuid.UUID
	EnvironmentID    uuid.UUID
	Declaration      computerDeclarationSelector
	DeclaredID       string
	Key              *string
	Secrets          []secretbinding.Binding
	IdempotencyKey   string
	Authorize        func(context.Context, pgx.Tx) error
}

type computerDeclarationSelector struct {
	Kind  computerDeclarationSelectorKind
	RunID uuid.UUID
}

type computerDeclarationSelectorKind string

const (
	computerDeclarationPromoted  computerDeclarationSelectorKind = "promoted"
	computerDeclarationRunPinned computerDeclarationSelectorKind = "run_pinned"
)

type computerCreateResult struct {
	ComputerID uuid.UUID
	Snapshot   api.ComputerSnapshot
	Replayed   bool
}

type computerCreateReceipt struct {
	Computer api.ComputerSnapshot `json:"computer"`
}

func (s *Server) createComputer(ctx context.Context, request computerCreateRequest) (computerCreateResult, error) {
	switch request.Declaration.Kind {
	case computerDeclarationPromoted:
		if request.Declaration.RunID != uuid.Nil() || request.Authorize != nil {
			return computerCreateResult{}, errComputerCreateInvalid
		}
	case computerDeclarationRunPinned:
		if request.Declaration.RunID == uuid.Nil() || request.Authorize == nil {
			return computerCreateResult{}, errComputerCreateInvalid
		}
	default:
		return computerCreateResult{}, errComputerCreateInvalid
	}
	placements, err := secretbinding.NormalizedPlacements(request.Secrets)
	if err != nil {
		return computerCreateResult{}, fmt.Errorf("%w: %v", errComputerCreateInvalid, err)
	}
	if err := validateComputerKey(request.Key); err != nil {
		return computerCreateResult{}, fmt.Errorf("%w: %v", errComputerCreateInvalid, err)
	}
	placementJSON, err := json.Marshal(placements)
	if err != nil {
		return computerCreateResult{}, fmt.Errorf("encode computer secret placements: %w", err)
	}
	var claimRequest idempotency.Request
	if request.IdempotencyKey != "" {
		fingerprint := idempotency.ComputerCreateFingerprint{
			Key: request.Key, Secrets: placementJSON,
		}
		switch request.Declaration.Kind {
		case computerDeclarationPromoted:
			claimRequest, err = idempotency.NewExternalComputerCreateRequest(
				request.EnvironmentID, request.DeclaredID,
				request.IdempotencyKey, fingerprint,
			)
		case computerDeclarationRunPinned:
			claimRequest, err = idempotency.NewRuntimeComputerCreateRequest(
				request.EnvironmentID, request.Declaration.RunID,
				request.DeclaredID, request.IdempotencyKey, fingerprint,
			)
		}
		if err != nil {
			return computerCreateResult{}, fmt.Errorf("%w: %v", errComputerCreateInvalid, err)
		}
	}

	var result computerCreateResult
	err = s.inTx(ctx, func(work *txWork) error {
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.tx); err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := idempotency.TransactionFor(work.tx)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.Status == "completed" {
				replayed, err := computerCreateResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				if request.SourceComputerID.Valid {
					if err := authorizeComputerSecretTarget(ctx, work.q, request.SourceComputerID, pgvalue.UUID(replayed.ComputerID)); err != nil {
						return err
					}
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.Status != "pending" {
				return errComputerCreateReceipt
			}
			claim = &acquired.Claim
		}

		var sandboxDefinition db.DeploymentDefinition
		switch request.Declaration.Kind {
		case computerDeclarationPromoted:
			sandboxDefinition, err = work.q.ResolveCurrentComputerDefinitionForCreate(
				ctx,
				db.ResolveCurrentComputerDefinitionForCreateParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					SandboxDeclaredID: request.DeclaredID,
				},
			)
		case computerDeclarationRunPinned:
			sandboxDefinition, err = work.q.ResolveRunPinnedComputerDefinitionForCreate(
				ctx,
				db.ResolveRunPinnedComputerDefinitionForCreateParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					RunID:             pgvalue.UUID(request.Declaration.RunID),
					SandboxDeclaredID: request.DeclaredID,
				},
			)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errComputerNotDeployed
		}
		if err != nil {
			return fmt.Errorf("%w: resolve promoted computer declaration: %v", errComputerAuthorityUnavailable, err)
		}

		secretIDs := make(map[string]pgtype.UUID, len(placements))
		secretNames := make([]string, 0, len(placements))
		for _, placement := range placements {
			if _, ok := secretIDs[placement.Name]; !ok {
				secretIDs[placement.Name] = pgtype.UUID{}
				secretNames = append(secretNames, placement.Name)
			}
		}
		sort.Strings(secretNames)
		if len(secretNames) > 0 {
			records, err := work.q.LockActiveSecretsByNameForComputerCreate(
				ctx,
				db.LockActiveSecretsByNameForComputerCreateParams{
					EnvironmentID: pgvalue.UUID(request.EnvironmentID),
					Names:         secretNames,
				},
			)
			if err != nil {
				return fmt.Errorf("lock computer secrets: %w", err)
			}
			for _, record := range records {
				secretIDs[record.Name] = record.ID
			}
			for _, name := range secretNames {
				if !secretIDs[name].Valid {
					return fmt.Errorf("%w: %s", errComputerSecretUnavailable, name)
				}
			}
		}

		computerID := uuid.NewV7()
		versionID := uuid.NewV7()
		key := pgtype.Text{}
		if request.Key != nil {
			key = pgtype.Text{String: *request.Key, Valid: true}
		}
		var createdComputerID pgtype.UUID
		var createdKey pgtype.Text
		var createdStatus string
		var createdLastActivityAt pgtype.Timestamptz
		var createdAt pgtype.Timestamptz
		var createdUpdatedAt pgtype.Timestamptz
		switch request.Declaration.Kind {
		case computerDeclarationPromoted:
			created, createErr := work.q.CreateComputerFromCurrentDeployment(
				ctx,
				db.CreateComputerFromCurrentDeploymentParams{
					ProjectID:              pgvalue.UUID(request.ProjectID),
					OrgID:                  pgvalue.UUID(request.OrgID),
					EnvironmentID:          pgvalue.UUID(request.EnvironmentID),
					DeploymentDefinitionID: sandboxDefinition.ID,
					SandboxDeclaredID:      request.DeclaredID,
					ID:                     pgvalue.UUID(computerID),
					InitialVersionID:       pgvalue.UUID(versionID),
					Key:                    key,
				},
			)
			err = createErr
			createdComputerID = created.ID
			createdKey = created.Key
			createdStatus = created.Status
			createdLastActivityAt = created.LastActivityAt
			createdAt = created.CreatedAt
			createdUpdatedAt = created.UpdatedAt
		case computerDeclarationRunPinned:
			created, createErr := work.q.CreateComputerFromRunDeployment(
				ctx,
				db.CreateComputerFromRunDeploymentParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					RunID:             pgvalue.UUID(request.Declaration.RunID),
					SandboxDeclaredID: request.DeclaredID,
					ID:                pgvalue.UUID(computerID),
					InitialVersionID:  pgvalue.UUID(versionID),
					Key:               key,
				},
			)
			err = createErr
			createdComputerID = created.ID
			createdKey = created.Key
			createdStatus = created.Status
			createdLastActivityAt = created.LastActivityAt
			createdAt = created.CreatedAt
			createdUpdatedAt = created.UpdatedAt
		}
		if err != nil {
			var postgresError *pgconn.PgError
			if errors.As(err, &postgresError) &&
				postgresError.ConstraintName == "computers_environment_key_uidx" &&
				request.Key != nil {
				return ComputerKeyConflictError{Key: *request.Key}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return errComputerNotDeployed
			}
			return fmt.Errorf("create computer: %w", err)
		}
		for _, placement := range placements {
			if placement.Mode != "protected" {
				continue
			}
			trust, err := s.secretProxy.GenerateProxyTrust(request.EnvironmentID, computerID, createdAt.Time)
			if err != nil {
				return err
			}
			count, err := work.q.InitializeComputerSecretCA(ctx, db.InitializeComputerSecretCAParams{
				EnvironmentID: pgvalue.UUID(trust.EnvironmentID), ComputerID: pgvalue.UUID(trust.ComputerID),
				Certificate: trust.Certificate, PrivateKeyNonce: trust.PrivateKeyNonce,
				PrivateKeyCiphertext: trust.PrivateKeyCiphertext, NotAfter: pgvalue.Timestamptz(trust.NotAfter),
			})
			if err != nil {
				return err
			}
			if count != 1 {
				return errComputerSecretUnavailable
			}
			break
		}
		for _, placement := range placements {
			placeholder, err := secretbinding.Placeholder(placement.Mode)
			if err != nil {
				return err
			}
			if _, err := work.q.CreateComputerSecret(ctx, db.CreateComputerSecretParams{
				ComputerID:      createdComputerID,
				EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
				PlacementKind:   placement.Kind,
				PlacementTarget: placement.Target,
				SecretID:        secretIDs[placement.Name],
				Mode:            placement.Mode, AllowedOrigins: placement.AllowedOrigins, Placeholder: placeholder,
			}); err != nil {
				return fmt.Errorf("create computer secret placement: %w", err)
			}
		}
		status, err := computerPublicStatus(createdStatus)
		if err != nil {
			return err
		}
		var snapshotKey *string
		if createdKey.Valid {
			value := createdKey.String
			snapshotKey = &value
		}
		snapshotSecrets := make([]secretbinding.Binding, 0, len(placements))
		for _, placement := range placements {
			item := secretbinding.Binding{Name: placement.Name}
			switch placement.Kind {
			case "env":
				item.Env = &secretbinding.Env{Name: placement.Target, Mode: placement.Mode, AllowedOrigins: placement.AllowedOrigins}
			case "file":
				item.File = &secretbinding.File{Path: placement.Target}
			default:
				return fmt.Errorf("unsupported computer secret placement %q", placement.Kind)
			}
			snapshotSecrets = append(snapshotSecrets, item)
		}
		snapshot := api.ComputerSnapshot{
			Residency:      "cold",
			ID:             computerID.String(),
			Key:            snapshotKey,
			SandboxID:      sandboxDefinition.DeclaredID,
			DeploymentID:   pgvalue.UUIDString(sandboxDefinition.DeploymentID),
			Status:         status,
			Secrets:        snapshotSecrets,
			LastActivityAt: pgvalue.Time(createdLastActivityAt),
			CreatedAt:      pgvalue.Time(createdAt),
			UpdatedAt:      pgvalue.Time(createdUpdatedAt),
		}
		result = computerCreateResult{ComputerID: computerID, Snapshot: snapshot}
		if claim != nil {
			receipt, err := json.Marshal(computerCreateReceipt{
				Computer: snapshot,
			})
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionFor(work.tx)
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func validateComputerKey(value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || len(*value) < 1 || len(*value) > 512 {
		return errors.New("computer key must contain 1 to 512 UTF-8 bytes")
	}
	if strings.TrimSpace(*value) != *value {
		return errors.New("computer key cannot begin or end with whitespace")
	}
	return nil
}

func computerCreateResultFromReceipt(raw []byte) (computerCreateResult, error) {
	var receipt computerCreateReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return computerCreateResult{}, errComputerCreateReceipt
	}
	computerID, err := ids.Parse(receipt.Computer.ID)
	if err != nil {
		return computerCreateResult{}, errComputerCreateReceipt
	}
	if ids.Validate(receipt.Computer.DeploymentID) != nil ||
		definition.ValidateSandboxDeclaredID(receipt.Computer.SandboxID) != nil ||
		receipt.Computer.Status != api.ComputerStatusAvailable ||
		receipt.Computer.LastActivityAt.IsZero() ||
		receipt.Computer.CreatedAt.IsZero() ||
		receipt.Computer.UpdatedAt.IsZero() ||
		validateComputerKey(receipt.Computer.Key) != nil {
		return computerCreateResult{}, errComputerCreateReceipt
	}
	if _, err := secretbinding.NormalizedPlacements(receipt.Computer.Secrets); err != nil {
		return computerCreateResult{}, errComputerCreateReceipt
	}
	return computerCreateResult{
		ComputerID: computerID,
		Snapshot:   receipt.Computer,
	}, nil
}
