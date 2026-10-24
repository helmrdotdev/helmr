package identity

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// APIKeyFilter selects API keys by status.
type APIKeyFilter string

const (
	APIKeyFilterActive  APIKeyFilter = "active"
	APIKeyFilterExpired APIKeyFilter = "expired"
	APIKeyFilterRevoked APIKeyFilter = "revoked"
	APIKeyFilterAll     APIKeyFilter = "all"
)

// ParseAPIKeyFilter parses an API key status filter; empty selects active keys.
func ParseAPIKeyFilter(value string) (APIKeyFilter, error) {
	switch filter := APIKeyFilter(value); filter {
	case "":
		return APIKeyFilterActive, nil
	case APIKeyFilterActive, APIKeyFilterExpired, APIKeyFilterRevoked, APIKeyFilterAll:
		return filter, nil
	default:
		return "", invalidInput("filter must be active, expired, revoked, or all")
	}
}

// APIKeyPosition is the sort key of the last API key on a previous page.
type APIKeyPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// APIKeyInput creates a machine identity, optionally assigning an existing Role
// atomically. A nil Role creates a key without workload access.
type APIKeyInput struct {
	Name          string
	Role          *APIKeyRole
	ExpiresInDays *int
}

// APIKeyRole selects an ordinary Role and its Agent targets for a new key.
// The key's organization and environment supply the assignment scope.
type APIKeyRole struct {
	RoleID               uuid.UUID
	ExpectedRoleRevision int64
	AgentsMode           string
	AgentIDs             []uuid.UUID
}

// IssuedAPIKey is an issued API key record with its raw key, which only the
// issue response carries.
type IssuedAPIKey struct {
	Record    db.APIKey
	Raw       string
	RoleNames []string
}

func authorizeAPIKeyManagement(manager auth.Principal, scope auth.Scope) error {
	if manager.Kind != auth.PrincipalKindSession || !manager.HasPermission(auth.PermissionAPIKeysManage, scope) {
		return ErrAPIKeyManagementRequired
	}
	return nil
}

// ListAPIKeys returns up to limit API keys of the environment matching the
// filter after the given position and whether more follow.
func ListAPIKeys(ctx context.Context, database db.DBTX, manager auth.Principal, scope auth.Scope, filter APIKeyFilter, limit int32, after *APIKeyPosition) ([]db.ListAPIKeysRow, bool, error) {
	q := db.New(database)
	if manager.Kind != auth.PrincipalKindSession {
		return nil, false, ErrAPIKeyManagementRequired
	}
	member, err := q.GetActiveOrgMemberByUser(ctx, db.GetActiveOrgMemberByUserParams{OrgID: pgvalue.UUID(manager.OrgID), UserID: pgvalue.UUID(manager.UserID)})
	if err != nil || pgvalue.MustUUIDValue(member.ID) != manager.MemberID {
		return nil, false, ErrAPIKeyManagementRequired
	}
	grants, err := access.MemberGrants(ctx, database, manager.OrgID, manager.MemberID)
	if err != nil {
		return nil, false, err
	}
	allowed := false
	for _, g := range grants {
		if g.EnvironmentID == "" && g.Permission == auth.CapabilityAccessManage || g.EnvironmentID != "" && g.EnvironmentID == scope.EnvironmentID && g.ProjectID == scope.ProjectID && g.Permission == auth.CapabilityKeysManage && g.AgentID == "" {
			allowed = true
		}
	}
	if !allowed {
		return nil, false, ErrAPIKeyManagementRequired
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return nil, false, err
	}
	params := db.ListAPIKeysParams{
		OrgID:         pgvalue.UUID(manager.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		StatusFilter:  string(filter),
		RowLimit:      limit + 1,
	}
	if after != nil {
		params.AfterCreatedAt = pgvalue.Timestamptz(after.CreatedAt)
		params.AfterID = pgvalue.UUID(after.ID)
	}
	rows, err := q.ListAPIKeys(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("list api keys: %w", err)
	}
	if len(rows) > int(limit) {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// IssueAPIKey creates a distinct machine principal. Reserved names are rejected.
func IssueAPIKey(ctx context.Context, database db.TxBeginner, issuer auth.Principal, scope auth.Scope, input APIKeyInput) (IssuedAPIKey, error) {
	if err := authorizeAPIKeyManagement(issuer, scope); err != nil {
		return IssuedAPIKey{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return IssuedAPIKey{}, invalidInput("name must be 1-64 characters and contain no control characters")
	}
	expiresAt := pgtype.Timestamptz{}
	if input.ExpiresInDays != nil {
		switch *input.ExpiresInDays {
		case 30, 90, 365:
		default:
			return IssuedAPIKey{}, invalidInput("expires_in_days must be 30, 90, or 365")
		}
		expiresAt = pgvalue.Timestamptz(time.Now().AddDate(0, 0, *input.ExpiresInDays))
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return IssuedAPIKey{}, err
	}
	generated, err := auth.GenerateAPIKey()
	if err != nil {
		return IssuedAPIKey{}, fmt.Errorf("generate api key: %w", err)
	}
	var record db.APIKey
	roleNames := []string{}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAPIKeyManager(ctx, tx, issuer, scope); err != nil {
			return err
		}
		q := db.New(tx)
		record, err = q.IssueAPIKey(ctx, db.IssueAPIKeyParams{
			ID:                pgvalue.UUID(uuid.NewV7()),
			OrgID:             pgvalue.UUID(issuer.OrgID),
			ProjectID:         projectID,
			EnvironmentID:     environmentID,
			CreatedByMemberID: pgvalue.UUID(issuer.MemberID),
			Name:              name,
			KeyPrefix:         generated.KeyPrefix,
			TokenHash:         generated.TokenHash,
			ExpiresAt:         expiresAt,
		})
		if err != nil {
			return apiKeyWriteError(err)
		}
		if input.Role != nil {
			keyID := pgvalue.MustUUIDValue(record.ID)
			project, environment := pgvalue.MustUUIDValue(projectID), pgvalue.MustUUIDValue(environmentID)
			grants, err := org.AssignRole(ctx, tx, org.ManagingMember{Actor: audit.FromPrincipal(issuer), OrgID: issuer.OrgID, UserID: issuer.UserID, MemberID: issuer.MemberID}, org.GrantInput{
				Subject: org.GrantSubject{Type: "api_key", APIKeyID: &keyID}, RoleID: input.Role.RoleID, ExpectedRoleRevision: input.Role.ExpectedRoleRevision,
				ProjectID: &project, EnvironmentID: &environment, AgentsMode: input.Role.AgentsMode, AgentIDs: input.Role.AgentIDs,
			})
			if err != nil {
				return err
			}
			for _, grant := range grants {
				roleNames = append(roleNames, grant.RoleName)
			}
		}
		return audit.Append(ctx, tx, audit.Event{Actor: audit.FromPrincipal(issuer), OrgID: issuer.OrgID, EnvironmentID: pgvalue.MustUUIDValue(environmentID), Action: "api_key.issued", TargetKind: "api_key", TargetID: pgvalue.MustUUIDValue(record.ID), Details: audit.APIKeyIssuedDetails{Name: record.Name, ExpiresAt: pgvalue.TimePtr(record.ExpiresAt)}})
	})
	if err != nil {
		return IssuedAPIKey{}, err
	}

	return IssuedAPIKey{Record: record, Raw: generated.Raw, RoleNames: roleNames}, nil
}

// RevokeAPIKey revokes an API key of the environment.
func RevokeAPIKey(ctx context.Context, database db.TxBeginner, manager auth.Principal, scope auth.Scope, id uuid.UUID) error {
	if err := authorizeAPIKeyManagement(manager, scope); err != nil {
		return err
	}
	projectID, environmentID, err := environmentIDs(scope)
	if err != nil {
		return err
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAPIKeyManager(ctx, tx, manager, scope); err != nil {
			return err
		}
		q := db.New(tx)
		revoked, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{
			OrgID:         pgvalue.UUID(manager.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(id),
		})
		if err != nil {
			return fmt.Errorf("revoke api key: %w", err)
		}
		if revoked == 0 {
			return ErrAPIKeyNotFound
		}
		return audit.Append(ctx, tx, audit.Event{Actor: audit.FromPrincipal(manager), OrgID: manager.OrgID, EnvironmentID: pgvalue.MustUUIDValue(environmentID), Action: "api_key.revoked", TargetKind: "api_key", TargetID: id})
	})
}

func environmentIDs(scope auth.Scope) (pgtype.UUID, pgtype.UUID, error) {
	projectID, err := ids.Parse(scope.ProjectID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := ids.Parse(scope.EnvironmentID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("environment id: %w", err)
	}
	return pgvalue.UUID(projectID), pgvalue.UUID(environmentID), nil
}

// APIKeyAuthenticator authenticates API keys and records their use.
type APIKeyAuthenticator struct {
	database db.DBTX
}

// NewAPIKeyAuthenticator returns an authenticator over the API keys in database.
func NewAPIKeyAuthenticator(database db.DBTX) APIKeyAuthenticator {
	return APIKeyAuthenticator{database: database}
}

// Authenticate resolves a raw API key to its principal and records its use. An
// unknown, revoked or expired key is auth.ErrUnauthenticated.
func (a APIKeyAuthenticator) Authenticate(ctx context.Context, rawKey string) (auth.Principal, error) {
	token := strings.TrimSpace(rawKey)
	if token == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	row, err := db.New(a.database).TouchActiveAPIKeyByTokenHash(ctx, auth.HashAPIKey(token))
	if isNoRows(err) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, fmt.Errorf("verify api key: %w", err)
	}
	orgID, err := pgvalue.UUIDValue(row.OrgID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key org id: %w", err)
	}
	projectID, err := pgvalue.UUIDValue(row.ProjectID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key project id: %w", err)
	}
	environmentID, err := pgvalue.UUIDValue(row.EnvironmentID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key environment id: %w", err)
	}
	apiKeyID, err := pgvalue.UUIDValue(row.ID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("api key id: %w", err)
	}
	grants, err := access.APIKeyGrants(ctx, a.database, orgID, apiKeyID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("read API key roles: %w", err)
	}
	return auth.Principal{
		OrgID:         orgID,
		APIKeyID:      apiKeyID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
		Kind:          auth.PrincipalKindAPIKey,
		Grants:        grants,
	}, nil
}

func apiKeyWriteError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return invalidInput("an unrevoked API key already reserves this name")
	}
	return err
}
func lockAPIKeyManager(ctx context.Context, tx pgx.Tx, manager auth.Principal, scope auth.Scope) error {
	if err := authorizeAPIKeyManagement(manager, scope); err != nil {
		return err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM org_members WHERE org_id=$1 AND id=$2 AND user_id=$3 AND removed_at IS NULL FOR UPDATE`, manager.OrgID, manager.MemberID, manager.UserID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyManagementRequired
		}
		return err
	}
	current, err := access.Current(ctx, tx, manager, scope)
	if err != nil {
		return ErrAPIKeyManagementRequired
	}
	return authorizeAPIKeyManagement(current, scope)
}

// APIKeyUpdate changes credential metadata without changing its identity or Roles.
type APIKeyUpdate struct {
	Name      string
	ExpiresAt *time.Time
}

func UpdateAPIKey(ctx context.Context, database db.TxBeginner, manager auth.Principal, scope auth.Scope, id uuid.UUID, input APIKeyUpdate) (db.APIKey, error) {
	var result db.APIKey
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return result, invalidInput("name must be 1-64 characters and contain no control characters")
	}
	expires := pgtype.Timestamptz{}
	if input.ExpiresAt != nil {
		if !input.ExpiresAt.After(time.Now()) {
			return result, invalidInput("expiry must be in the future")
		}
		expires = pgvalue.Timestamptz(*input.ExpiresAt)
	}
	project, environment, err := environmentIDs(scope)
	if err != nil {
		return result, err
	}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAPIKeyManager(ctx, tx, manager, scope); err != nil {
			return err
		}
		q := db.New(tx)
		before, err := q.LockActiveAPIKey(ctx, db.LockActiveAPIKeyParams{OrgID: pgvalue.UUID(manager.OrgID), ProjectID: project, EnvironmentID: environment, ID: pgvalue.UUID(id)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyNotFound
		}
		if err != nil {
			return err
		}
		result, err = q.UpdateActiveAPIKey(ctx, db.UpdateActiveAPIKeyParams{OrgID: before.OrgID, ProjectID: project, EnvironmentID: environment, ID: before.ID, Name: name, ExpiresAt: expires})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyNotFound
		}
		if err != nil {
			return apiKeyWriteError(err)
		}
		return audit.Append(ctx, tx, audit.Event{Actor: audit.FromPrincipal(manager), OrgID: manager.OrgID, EnvironmentID: pgvalue.MustUUIDValue(environment), Action: "api_key.updated", TargetKind: "api_key", TargetID: id, Details: audit.APIKeyChangedDetails{Before: apiKeyPolicy(before), After: apiKeyPolicy(result)}})
	})
	return result, err
}

func RotateAPIKey(ctx context.Context, database db.TxBeginner, manager auth.Principal, scope auth.Scope, id uuid.UUID) (IssuedAPIKey, error) {
	var result IssuedAPIKey
	project, environment, err := environmentIDs(scope)
	if err != nil {
		return result, err
	}
	generated, err := auth.GenerateAPIKey()
	if err != nil {
		return result, err
	}
	err = db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAPIKeyManager(ctx, tx, manager, scope); err != nil {
			return err
		}
		q := db.New(tx)
		before, err := q.LockActiveAPIKey(ctx, db.LockActiveAPIKeyParams{OrgID: pgvalue.UUID(manager.OrgID), ProjectID: project, EnvironmentID: environment, ID: pgvalue.UUID(id)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyNotFound
		}
		if err != nil {
			return err
		}
		result.Record, err = q.RotateActiveAPIKey(ctx, db.RotateActiveAPIKeyParams{OrgID: before.OrgID, ProjectID: project, EnvironmentID: environment, ID: before.ID, TokenHash: generated.TokenHash, KeyPrefix: generated.KeyPrefix})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyNotFound
		}
		if err != nil {
			return err
		}
		result.RoleNames, err = q.ListAPIKeyRoleNames(ctx, db.ListAPIKeyRoleNamesParams{OrgID: before.OrgID, ApiKeyID: before.ID})
		if err != nil {
			return err
		}
		return audit.Append(ctx, tx, audit.Event{Actor: audit.FromPrincipal(manager), OrgID: manager.OrgID, EnvironmentID: pgvalue.MustUUIDValue(environment), Action: "api_key.rotated", TargetKind: "api_key", TargetID: id})
	})
	if err != nil {
		return IssuedAPIKey{}, err
	}
	result.Raw = generated.Raw
	return result, nil
}
func apiKeyPolicy(key db.APIKey) audit.APIKeyPolicy {
	return audit.APIKeyPolicy{Name: key.Name, ExpiresAt: pgvalue.TimePtr(key.ExpiresAt)}
}
