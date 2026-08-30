package auth

import (
	"errors"
	"strings"
	"time"

	"uuid"
)

type EpochExchangeInput struct {
	ServiceID uuid.UUID
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
}

func (input EpochExchangeInput) Validate() error {
	if input.ServiceID == uuid.Nil() {
		return errors.New("service_id is required")
	}
	return nil
}

// Claims validates the authority returned by the epoch-exchange transaction.
// ServiceID is deliberately not copied into the JWT: it is the idempotency key
// for the transaction that returned authority.WorkerEpoch.
func (authority WorkerTokenAuthority) Claims(input EpochExchangeInput, issuedAt, expiresAt time.Time) (WorkerClaims, error) {
	if err := input.Validate(); err != nil {
		return WorkerClaims{}, err
	}
	if strings.TrimSpace(authority.WorkerGroupID) == "" || strings.TrimSpace(authority.WorkerGroupID) != authority.WorkerGroupID {
		return WorkerClaims{}, errors.New("worker_group_id must be nonempty and canonical")
	}
	if authority.WorkerInstanceID == uuid.Nil() {
		return WorkerClaims{}, errors.New("worker_instance_id is required")
	}
	if authority.CredentialID == uuid.Nil() {
		return WorkerClaims{}, errors.New("credential_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return WorkerClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	return WorkerClaims{
		WorkerGroupID: authority.WorkerGroupID, WorkerInstanceID: authority.WorkerInstanceID.String(),
		CredentialID: authority.CredentialID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}
