package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/ids"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type PrincipalKind string

const (
	PrincipalKindAPIKey  PrincipalKind = "api_key"
	PrincipalKindSession PrincipalKind = "session"
)

type Role string

const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

type Principal struct {
	OrgID         uuid.UUID
	UserID        uuid.UUID
	APIKeyID      uuid.UUID
	SessionID     uuid.UUID
	ProjectID     string
	EnvironmentID string
	Kind          PrincipalKind
	Role          Role
	Admin         bool
	Permissions   []Permission
}

type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (Principal, error)
}

func (a Principal) EnvironmentScope() (Scope, bool) {
	if a.ProjectID == "" || a.EnvironmentID == "" {
		return Scope{}, false
	}
	projectID, err := ids.Parse(a.ProjectID)
	if err != nil || projectID == uuid.Nil() {
		return Scope{}, false
	}
	environmentID, err := ids.Parse(a.EnvironmentID)
	if err != nil || environmentID == uuid.Nil() {
		return Scope{}, false
	}
	return Scope{OrgID: a.OrgID, ProjectID: a.ProjectID, EnvironmentID: a.EnvironmentID}, true
}

func HashAPIKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
