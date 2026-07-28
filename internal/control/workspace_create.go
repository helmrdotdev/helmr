package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxWorkspaceSecrets = 64

var workspaceSecretEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	errWorkspaceCreateInvalid        = errors.New("Workspace create request is invalid")
	errWorkspaceNotDeployed          = errors.New("Workspace declaration is not deployed")
	errWorkspaceSecretUnavailable    = errors.New("Workspace Secret is unavailable")
	errWorkspaceCreateReceipt        = errors.New("Workspace create idempotency receipt is invalid")
	errWorkspaceAuthorityUnavailable = errors.New("Workspace authority is unavailable")
)

type WorkspaceKeyConflictError struct {
	Key string
}

func (e WorkspaceKeyConflictError) Error() string {
	return fmt.Sprintf("Workspace key %q is already in use", e.Key)
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
	WorkspaceID       uuid.UUID
	WorkspacePublicID string
	Replayed          bool
}

type workspaceCreateReceipt struct {
	WorkspaceID       string `json:"workspaceId"`
	WorkspacePublicID string `json:"workspacePublicId"`
}

type workspaceSecretPlacement struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

func (s *Server) createWorkspace(ctx context.Context, request workspaceCreateRequest) (workspaceCreateResult, error) {
	switch request.Declaration.Kind {
	case workspaceDeclarationPromoted:
		if request.Declaration.RunID != uuid.Nil || request.Authorize != nil {
			return workspaceCreateResult{}, errWorkspaceCreateInvalid
		}
	case workspaceDeclarationRunPinned:
		if request.Declaration.RunID == uuid.Nil || request.Authorize == nil {
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
		return workspaceCreateResult{}, fmt.Errorf("encode Workspace Secret placements: %w", err)
	}
	var claimRequest idempotency.Request
	if request.IdempotencyKey != "" {
		claimRequest, err = idempotency.NewWorkspaceCreateRequest(
			request.EnvironmentID,
			request.DeclaredID,
			request.IdempotencyKey,
			idempotency.WorkspaceCreateFingerprint{Key: request.Key, Secrets: placementJSON},
		)
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
					EnvironmentID:       pgvalue.UUID(request.EnvironmentID),
					WorkspaceDeclaredID: request.DeclaredID,
				},
			)
		case workspaceDeclarationRunPinned:
			definition, err = work.q.ResolveRunPinnedWorkspaceDefinitionForCreate(
				ctx,
				db.ResolveRunPinnedWorkspaceDefinitionForCreateParams{
					EnvironmentID:       pgvalue.UUID(request.EnvironmentID),
					RunID:               pgvalue.UUID(request.Declaration.RunID),
					WorkspaceDeclaredID: request.DeclaredID,
				},
			)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errWorkspaceNotDeployed
		}
		if err != nil {
			return fmt.Errorf("%w: resolve promoted Workspace declaration: %v", errWorkspaceAuthorityUnavailable, err)
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
		for _, name := range secretNames {
			record, err := work.q.LockActiveSecretByNameForWorkspaceCreate(
				ctx,
				db.LockActiveSecretByNameForWorkspaceCreateParams{
					EnvironmentID: pgvalue.UUID(request.EnvironmentID),
					Name:          name,
				},
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %s", errWorkspaceSecretUnavailable, name)
			}
			if err != nil {
				return fmt.Errorf("lock Workspace Secret %q: %w", name, err)
			}
			secretIDs[name] = record.ID
		}

		workspaceID := uuid.Must(uuid.NewV7())
		versionID := uuid.Must(uuid.NewV7())
		workspacePublicID, err := publicid.New(publicid.Workspace)
		if err != nil {
			return err
		}
		versionPublicID, err := publicid.New(publicid.WorkspaceVersion)
		if err != nil {
			return err
		}
		key := pgtype.Text{}
		if request.Key != nil {
			key = pgtype.Text{String: *request.Key, Valid: true}
		}
		var createdWorkspaceID pgtype.UUID
		switch request.Declaration.Kind {
		case workspaceDeclarationPromoted:
			created, createErr := work.q.CreateWorkspaceFromCurrentDeployment(
				ctx,
				db.CreateWorkspaceFromCurrentDeploymentParams{
					ProjectID:              pgvalue.UUID(request.ProjectID),
					OrgID:                  pgvalue.UUID(request.OrgID),
					EnvironmentID:          pgvalue.UUID(request.EnvironmentID),
					DeploymentDefinitionID: definition.ID,
					WorkspaceDeclaredID:    request.DeclaredID,
					ID:                     pgvalue.UUID(workspaceID),
					PublicID:               workspacePublicID,
					InitialVersionID:       pgvalue.UUID(versionID),
					Key:                    key,
					InitialVersionPublicID: versionPublicID,
				},
			)
			err = createErr
			createdWorkspaceID = created.ID
		case workspaceDeclarationRunPinned:
			created, createErr := work.q.CreateWorkspaceFromRunDeployment(
				ctx,
				db.CreateWorkspaceFromRunDeploymentParams{
					EnvironmentID:          pgvalue.UUID(request.EnvironmentID),
					RunID:                  pgvalue.UUID(request.Declaration.RunID),
					WorkspaceDeclaredID:    request.DeclaredID,
					ID:                     pgvalue.UUID(workspaceID),
					PublicID:               workspacePublicID,
					InitialVersionID:       pgvalue.UUID(versionID),
					Key:                    key,
					InitialVersionPublicID: versionPublicID,
				},
			)
			err = createErr
			createdWorkspaceID = created.ID
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
			return fmt.Errorf("create Workspace: %w", err)
		}
		for _, placement := range placements {
			if _, err := work.q.CreateWorkspaceSecret(ctx, db.CreateWorkspaceSecretParams{
				WorkspaceID:     createdWorkspaceID,
				EnvironmentID:   pgvalue.UUID(request.EnvironmentID),
				PlacementKind:   placement.Kind,
				PlacementTarget: placement.Target,
				SecretID:        secretIDs[placement.Name],
			}); err != nil {
				return fmt.Errorf("create Workspace Secret placement: %w", err)
			}
		}
		result = workspaceCreateResult{
			WorkspaceID:       workspaceID,
			WorkspacePublicID: workspacePublicID,
		}
		if claim != nil {
			receipt, err := json.Marshal(workspaceCreateReceipt{
				WorkspaceID:       workspaceID.String(),
				WorkspacePublicID: workspacePublicID,
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

func normalizeWorkspaceSecretPlacements(input []api.WorkspaceSecret) ([]workspaceSecretPlacement, error) {
	if len(input) > maxWorkspaceSecrets {
		return nil, fmt.Errorf("at most %d Workspace Secret placements are allowed", maxWorkspaceSecrets)
	}
	placements := make([]workspaceSecretPlacement, 0, len(input))
	envTargets := make(map[string]struct{}, len(input))
	fileTargets := make([]string, 0, len(input))
	for _, value := range input {
		if err := api.ValidateWorkspaceSecret(value); err != nil {
			return nil, err
		}
		if err := secret.ValidateName(value.Name); err != nil {
			return nil, err
		}
		placement := workspaceSecretPlacement{Name: value.Name}
		switch {
		case value.Env != "":
			if !workspaceSecretEnvPattern.MatchString(value.Env) || strings.HasPrefix(value.Env, "HELMR_") {
				return nil, fmt.Errorf("invalid or reserved Workspace Secret environment target %q", value.Env)
			}
			if _, exists := envTargets[value.Env]; exists {
				return nil, fmt.Errorf("duplicate Workspace Secret environment target %q", value.Env)
			}
			envTargets[value.Env] = struct{}{}
			placement.Kind = "env"
			placement.Target = value.Env
		case value.File != "":
			if err := validateWorkspaceSecretFile(value.File); err != nil {
				return nil, err
			}
			fileTargets = append(fileTargets, value.File)
			placement.Kind = "file"
			placement.Target = value.File
		}
		placements = append(placements, placement)
	}
	sort.Slice(fileTargets, func(i, j int) bool { return fileTargets[i] < fileTargets[j] })
	for index, target := range fileTargets {
		if index > 0 {
			previous := fileTargets[index-1]
			if target == previous || strings.HasPrefix(target, previous+"/") {
				return nil, fmt.Errorf("conflicting Workspace Secret file targets %q and %q", previous, target)
			}
		}
	}
	sort.Slice(placements, func(i, j int) bool {
		if placements[i].Kind != placements[j].Kind {
			return placements[i].Kind < placements[j].Kind
		}
		if placements[i].Target != placements[j].Target {
			return placements[i].Target < placements[j].Target
		}
		return placements[i].Name < placements[j].Name
	})
	return placements, nil
}

func validateWorkspaceSecretFile(value string) error {
	if !utf8.ValidString(value) || len(value) > 4096 || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("invalid Workspace Secret file target %q", value)
	}
	if !strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "/" {
		return fmt.Errorf("Workspace Secret file target %q must be a canonical absolute path", value)
	}
	if value == "/workspace" || strings.HasPrefix(value, "/workspace/") {
		return fmt.Errorf("Workspace Secret file target %q overlaps the durable Workspace root", value)
	}
	return nil
}

func validateWorkspaceKey(value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || len(*value) < 1 || len(*value) > 512 {
		return errors.New("Workspace key must contain 1 to 512 UTF-8 bytes")
	}
	if strings.TrimSpace(*value) != *value {
		return errors.New("Workspace key cannot begin or end with whitespace")
	}
	return nil
}

func workspaceCreateResultFromReceipt(raw []byte) (workspaceCreateResult, error) {
	var receipt workspaceCreateReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	workspaceID, err := uuid.Parse(receipt.WorkspaceID)
	if err != nil || publicid.ValidateFor(publicid.Workspace, receipt.WorkspacePublicID) != nil {
		return workspaceCreateResult{}, errWorkspaceCreateReceipt
	}
	return workspaceCreateResult{
		WorkspaceID:       workspaceID,
		WorkspacePublicID: receipt.WorkspacePublicID,
	}, nil
}
