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
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errWorkspaceCreateInvalid        = errors.New("workspace create request is invalid")
	errWorkspaceNotDeployed          = errors.New("workspace declaration is not deployed")
	errWorkspaceSecretUnavailable    = errors.New("workspace secret is unavailable")
	errWorkspaceCreateReceipt        = errors.New("workspace create idempotency receipt is invalid")
	errWorkspaceAuthorityUnavailable = errors.New("workspace authority is unavailable")
)

type WorkspaceKeyConflictError struct {
	Key string
}

func (e WorkspaceKeyConflictError) Error() string {
	return fmt.Sprintf("workspace key %q is already in use", e.Key)
}

type workspaceCreateRequest struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	EnvironmentID  uuid.UUID
	Declaration    workspaceDeclarationSelector
	DeclaredID     string
	Key            *string
	Secrets        []api.WorkspaceSecret
	IdempotencyKey string
	Authorize      func(context.Context, db.Querier) error
}

type workspaceDeclarationSelector struct {
	Kind  workspaceDeclarationSelectorKind
	RunID uuid.UUID
}

type workspaceDeclarationSelectorKind string

const (
	workspaceDeclarationPromoted  workspaceDeclarationSelectorKind = "promoted"
	workspaceDeclarationRunPinned workspaceDeclarationSelectorKind = "run_pinned"
)

type workspaceCreateResult struct {
	WorkspaceID uuid.UUID
	Snapshot    api.WorkspaceSnapshot
	Replayed    bool
}

type workspaceCreateReceipt struct {
	Workspace api.WorkspaceSnapshot `json:"workspace"`
}

func (s *Server) createWorkspace(ctx context.Context, request workspaceCreateRequest) (workspaceCreateResult, error) {
	switch request.Declaration.Kind {
	case workspaceDeclarationPromoted:
		if request.Declaration.RunID != uuid.Nil() || request.Authorize != nil {
			return workspaceCreateResult{}, errWorkspaceCreateInvalid
		}
	case workspaceDeclarationRunPinned:
		if request.Declaration.RunID == uuid.Nil() || request.Authorize == nil {
			return workspaceCreateResult{}, errWorkspaceCreateInvalid
		}
	default:
		return workspaceCreateResult{}, errWorkspaceCreateInvalid
	}
	placements, err := normalizeWorkspaceSecretPlacements(request.Secrets)
	if err != nil {
		return workspaceCreateResult{}, fmt.Errorf("%w: %v", errWorkspaceCreateInvalid, err)
	}
	if err := validateWorkspaceKey(request.Key); err != nil {
		return workspaceCreateResult{}, fmt.Errorf("%w: %v", errWorkspaceCreateInvalid, err)
	}
	placementJSON, err := json.Marshal(placements)
	if err != nil {
		return workspaceCreateResult{}, fmt.Errorf("encode workspace secret placements: %w", err)
	}
	var claimRequest idempotency.Request
	if request.IdempotencyKey != "" {
		fingerprint := idempotency.WorkspaceCreateFingerprint{
			Key: request.Key, Secrets: placementJSON,
		}
		switch request.Declaration.Kind {
		case workspaceDeclarationPromoted:
			claimRequest, err = idempotency.NewExternalWorkspaceCreateRequest(
				request.EnvironmentID, request.DeclaredID,
				request.IdempotencyKey, fingerprint,
			)
		case workspaceDeclarationRunPinned:
			claimRequest, err = idempotency.NewRuntimeWorkspaceCreateRequest(
				request.EnvironmentID, request.Declaration.RunID,
				request.DeclaredID, request.IdempotencyKey, fingerprint,
			)
		}
		if err != nil {
			return workspaceCreateResult{}, fmt.Errorf("%w: %v", errWorkspaceCreateInvalid, err)
		}
	}

	var result workspaceCreateResult
	err = s.inTx(ctx, func(work *txWork) error {
		if request.Authorize != nil {
			if err := request.Authorize(ctx, work.q); err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		if claimRequest != nil {
			claims, err := idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := workspaceCreateResultFromReceipt(acquired.Claim.Receipt)
				if err != nil {
					return err
				}
				replayed.Replayed = true
				result = replayed
				return nil
			}
			if acquired.Claim.State != "pending" {
				return errWorkspaceCreateReceipt
			}
			claim = &acquired.Claim
		}

		var definition db.DeploymentDefinition
		switch request.Declaration.Kind {
		case workspaceDeclarationPromoted:
			definition, err = work.q.ResolveCurrentWorkspaceDefinitionForCreate(
				ctx,
				db.ResolveCurrentWorkspaceDefinitionForCreateParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					SandboxDeclaredID: request.DeclaredID,
				},
			)
		case workspaceDeclarationRunPinned:
			definition, err = work.q.ResolveRunPinnedWorkspaceDefinitionForCreate(
				ctx,
				db.ResolveRunPinnedWorkspaceDefinitionForCreateParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					RunID:             pgvalue.UUID(request.Declaration.RunID),
					SandboxDeclaredID: request.DeclaredID,
				},
			)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errWorkspaceNotDeployed
		}
		if err != nil {
			return fmt.Errorf("%w: resolve promoted workspace declaration: %v", errWorkspaceAuthorityUnavailable, err)
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
			records, err := work.q.LockActiveSecretsByNameForWorkspaceCreate(
				ctx,
				db.LockActiveSecretsByNameForWorkspaceCreateParams{
					EnvironmentID: pgvalue.UUID(request.EnvironmentID),
					Names:         secretNames,
				},
			)
			if err != nil {
				return fmt.Errorf("lock workspace secrets: %w", err)
			}
			for _, record := range records {
				secretIDs[record.Name] = record.ID
			}
			for _, name := range secretNames {
				if !secretIDs[name].Valid {
					return fmt.Errorf("%w: %s", errWorkspaceSecretUnavailable, name)
				}
			}
		}

		workspaceID := uuid.NewV7()
		versionID := uuid.NewV7()
		key := pgtype.Text{}
		if request.Key != nil {
			key = pgtype.Text{String: *request.Key, Valid: true}
		}
		var createdWorkspaceID pgtype.UUID
		var createdKey pgtype.Text
		var createdState string
		var createdLastActivityAt pgtype.Timestamptz
		var createdAt pgtype.Timestamptz
		var createdUpdatedAt pgtype.Timestamptz
		switch request.Declaration.Kind {
		case workspaceDeclarationPromoted:
			created, createErr := work.q.CreateWorkspaceFromCurrentDeployment(
				ctx,
				db.CreateWorkspaceFromCurrentDeploymentParams{
					ProjectID:              pgvalue.UUID(request.ProjectID),
					OrgID:                  pgvalue.UUID(request.OrgID),
					EnvironmentID:          pgvalue.UUID(request.EnvironmentID),
					DeploymentDefinitionID: definition.ID,
					SandboxDeclaredID:      request.DeclaredID,
					ID:                     pgvalue.UUID(workspaceID),
					InitialVersionID:       pgvalue.UUID(versionID),
					Key:                    key,
				},
			)
			err = createErr
			createdWorkspaceID = created.ID
			createdKey = created.Key
			createdState = created.State
			createdLastActivityAt = created.LastActivityAt
			createdAt = created.CreatedAt
			createdUpdatedAt = created.UpdatedAt
		case workspaceDeclarationRunPinned:
			created, createErr := work.q.CreateWorkspaceFromRunDeployment(
				ctx,
				db.CreateWorkspaceFromRunDeploymentParams{
					EnvironmentID:     pgvalue.UUID(request.EnvironmentID),
					RunID:             pgvalue.UUID(request.Declaration.RunID),
					SandboxDeclaredID: request.DeclaredID,
					ID:                pgvalue.UUID(workspaceID),
					InitialVersionID:  pgvalue.UUID(versionID),
					Key:               key,
				},
			)
			err = createErr
			createdWorkspaceID = created.ID
			createdKey = created.Key
			createdState = created.State
			createdLastActivityAt = created.LastActivityAt
			createdAt = created.CreatedAt
			createdUpdatedAt = created.UpdatedAt
		}
		if err != nil {
			var postgresError *pgconn.PgError
			if errors.As(err, &postgresError) &&
				postgresError.ConstraintName == "workspaces_environment_key_uidx" &&
				request.Key != nil {
				return WorkspaceKeyConflictError{Key: *request.Key}
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return errWorkspaceNotDeployed
			}
			return fmt.Errorf("create workspace: %w", err)
		}
		for _, placement := range placements {
			if _, err := work.q.CreateWorkspaceSecret(ctx, db.CreateWorkspaceSecretParams{
				WorkspaceID:     createdWorkspaceID,
				EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
				PlacementKind:   placement.Kind,
				PlacementTarget: placement.Target,
				SecretID:        secretIDs[placement.Name],
			}); err != nil {
				return fmt.Errorf("create workspace secret placement: %w", err)
			}
		}
		status, err := workspacePublicStatus(createdState)
		if err != nil {
			return err
		}
		var snapshotKey *string
		if createdKey.Valid {
			value := createdKey.String
			snapshotKey = &value
		}
		snapshotSecrets := make([]api.WorkspaceSecret, 0, len(placements))
		for _, placement := range placements {
			item := api.WorkspaceSecret{Name: placement.Name}
			switch placement.Kind {
			case "env":
				item.Env = placement.Target
			case "file":
				item.File = placement.Target
			default:
				return fmt.Errorf("unsupported workspace secret placement %q", placement.Kind)
			}
			snapshotSecrets = append(snapshotSecrets, item)
		}
		snapshot := api.WorkspaceSnapshot{
			ID:             workspaceID.String(),
			Key:            snapshotKey,
			SandboxID:      definition.DeclaredID,
			DeploymentID:   pgvalue.UUIDString(definition.DeploymentID),
			Status:         status,
			Secrets:        snapshotSecrets,
			LastActivityAt: pgvalue.Time(createdLastActivityAt),
			CreatedAt:      pgvalue.Time(createdAt),
			UpdatedAt:      pgvalue.Time(createdUpdatedAt),
		}
		result = workspaceCreateResult{WorkspaceID: workspaceID, Snapshot: snapshot}
		if claim != nil {
			receipt, err := json.Marshal(workspaceCreateReceipt{
				Workspace: snapshot,
			})
			if err != nil {
				return err
			}
			claims, err := idempotency.TransactionForQueries(work.q)
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

func normalizeWorkspaceSecretPlacements(input []api.WorkspaceSecret) ([]workspace.SecretPlacement, error) {
	placements := make([]workspace.SecretPlacement, 0, len(input))
	for _, value := range input {
		if err := api.ValidateWorkspaceSecret(value); err != nil {
			return nil, err
		}
		placement := workspace.SecretPlacement{Name: value.Name}
		switch {
		case value.Env != "":
			placement.Kind = "env"
			placement.Target = value.Env
		case value.File != "":
			placement.Kind = "file"
			placement.Target = value.File
		}
		placements = append(placements, placement)
	}
	return workspace.NormalizeSecretPlacements(placements)
}

func validateWorkspaceKey(value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || len(*value) < 1 || len(*value) > 512 {
		return errors.New("workspace key must contain 1 to 512 UTF-8 bytes")
	}
	if strings.TrimSpace(*value) != *value {
		return errors.New("workspace key cannot begin or end with whitespace")
	}
	return nil
}

func workspaceCreateResultFromReceipt(raw []byte) (workspaceCreateResult, error) {
	var receipt workspaceCreateReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	workspaceID, err := ids.Parse(receipt.Workspace.ID)
	if err != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	if ids.Validate(receipt.Workspace.DeploymentID) != nil ||
		api.ValidateSandboxDeclaredID(receipt.Workspace.SandboxID) != nil ||
		receipt.Workspace.Status != api.WorkspaceStatusAvailable ||
		receipt.Workspace.LastActivityAt.IsZero() ||
		receipt.Workspace.CreatedAt.IsZero() ||
		receipt.Workspace.UpdatedAt.IsZero() ||
		validateWorkspaceKey(receipt.Workspace.Key) != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	if _, err := normalizeWorkspaceSecretPlacements(receipt.Workspace.Secrets); err != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	return workspaceCreateResult{
		WorkspaceID: workspaceID,
		Snapshot:    receipt.Workspace,
	}, nil
}
