// Package imagecache defines the provider-neutral trusted-plane boundary for
// automatic Workspace-image layer cache provisioning and retirement.
package imagecache

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Target is non-secret and attempt-local. Provider credentials and provider
// resource locators do not cross this boundary.
type Target struct {
	Authority string
	Username  string
	Ref       string
}

// Credential is one attempt-local registry credential. Call Clear as soon as
// its consumer has copied the value into the VM-local credential provider.
type Credential struct {
	Authority string
	Username  string
	Password  []byte
}

func (credential *Credential) Clear() {
	if credential == nil {
		return
	}
	for i := range credential.Password {
		credential.Password[i] = 0
	}
	credential.Password = nil
}

// CredentialProvider obtains an attempt-local credential for a previously
// admitted Target. Provider authority never crosses this interface.
type CredentialProvider interface {
	Fetch(context.Context, Target) (Credential, error)
}

// UnavailableError identifies an expected cache-transport failure. The
// requested operation remains prefer, but the current attempt may run cold.
type UnavailableError struct {
	Operation string
	Err       error
}

func (err *UnavailableError) Error() string {
	return fmt.Sprintf("image cache %s unavailable: %v", err.Operation, err.Err)
}

func (err *UnavailableError) Unwrap() error { return err.Err }

func IsUnavailable(err error) bool {
	var unavailable *UnavailableError
	return errors.As(err, &unavailable)
}

// ContractError is a hard configuration or assignment mismatch. It must not
// be converted into a provider retry or destructive repository replacement.
type ContractError struct {
	Message string
}

func (err *ContractError) Error() string { return "image cache contract: " + err.Message }

// RepositoryProvisioner derives and ensures the cache transport selected by
// Control after durable Workspace-image admission.
type RepositoryProvisioner interface {
	Target(environmentID uuid.UUID, cacheScope string) (Target, error)
	Ensure(context.Context, Target) error
}

// RepositoryRetirer removes the one provider resource deterministically owned
// by an Environment. A nil result means absence has been proven.
type RepositoryRetirer interface {
	Retire(context.Context, uuid.UUID) error
}
