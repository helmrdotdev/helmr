package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrWorkerRoleIntersectionEmpty = errors.New("worker role intersection is empty")

type WorkerRoles struct {
	Run   bool
	Build bool
}

type EpochExchangeInput struct {
	ServiceID       uuid.UUID
	SupervisorRoles WorkerRoles
	ProtocolVersion string
}

// WorkerTokenAuthority is loaded by the epoch-exchange transaction. It keeps
// identity and policy authority out of the supervisor request body.
type WorkerTokenAuthority struct {
	WorkerGroupID     string
	WorkerInstanceID  uuid.UUID
	CredentialID      uuid.UUID
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
	CredentialRoles   WorkerRoles
	GroupRoles        WorkerRoles
}

func (input EpochExchangeInput) Validate() error {
	if input.ServiceID == uuid.Nil {
		return errors.New("service_id is required")
	}
	if input.ProtocolVersion != WorkerProtocolVersion {
		return fmt.Errorf("protocol_version must be %q", WorkerProtocolVersion)
	}
	if !input.SupervisorRoles.Run && !input.SupervisorRoles.Build {
		return errors.New("supervisor must support at least one worker role")
	}
	return nil
}

// Claims intersects credential, group, and supervisor roles. ServiceID is
// deliberately validated but not copied into the JWT: it is the idempotency
// key for the transaction that returned authority.WorkerEpoch.
func (authority WorkerTokenAuthority) Claims(input EpochExchangeInput, issuedAt, expiresAt time.Time) (WorkerClaims, error) {
	if err := input.Validate(); err != nil {
		return WorkerClaims{}, err
	}
	if strings.TrimSpace(authority.WorkerGroupID) == "" || strings.TrimSpace(authority.WorkerGroupID) != authority.WorkerGroupID {
		return WorkerClaims{}, errors.New("worker_group_id must be nonempty and canonical")
	}
	if authority.WorkerInstanceID == uuid.Nil {
		return WorkerClaims{}, errors.New("worker_instance_id is required")
	}
	if authority.CredentialID == uuid.Nil {
		return WorkerClaims{}, errors.New("credential_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return WorkerClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	roles := make([]string, 0, 2)
	if authority.CredentialRoles.Build && authority.GroupRoles.Build && input.SupervisorRoles.Build {
		roles = append(roles, WorkerRoleBuild)
	}
	if authority.CredentialRoles.Run && authority.GroupRoles.Run && input.SupervisorRoles.Run {
		roles = append(roles, WorkerRoleRun)
	}
	if len(roles) == 0 {
		return WorkerClaims{}, ErrWorkerRoleIntersectionEmpty
	}

	return WorkerClaims{
		WorkerGroupID: authority.WorkerGroupID, WorkerInstanceID: authority.WorkerInstanceID.String(),
		CredentialID: authority.CredentialID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		Roles: roles, ProtocolVersion: input.ProtocolVersion, IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}
