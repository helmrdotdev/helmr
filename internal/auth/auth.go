package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/ids"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type PrincipalKind string

const (
	PrincipalKindAPIKey   PrincipalKind = "api_key"
	PrincipalKindSession  PrincipalKind = "session"
	PrincipalKindOperator PrincipalKind = "operator"
)

type Principal struct {
	CredentialVersion int64
	OperatorID        uuid.UUID
	SlackLinkID       uuid.UUID
	IdentityID        uuid.UUID
	PasskeyID         uuid.UUID
	AuthenticatedAt   time.Time
	// ProofProvider is the provider of IdentityID ("email", "github"); empty
	// for a passkey.
	ProofProvider string
	// SSOProofID and SSOAuthenticatedAt are the browser session's live SSO
	// proof for OrgID, or the proof a CLI session was approved with.
	// SSORequired reports that OrgID admits only its SSO proof.
	SSOProofID         uuid.UUID
	SSOAuthenticatedAt time.Time
	SSORequired        bool
	Channel            string

	OrgID         uuid.UUID
	UserID        uuid.UUID
	MemberID      uuid.UUID
	IsOwner       bool
	APIKeyID      uuid.UUID
	SessionID     uuid.UUID
	ProjectID     string
	EnvironmentID string
	Kind          PrincipalKind
	Grants        []PermissionGrant
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
