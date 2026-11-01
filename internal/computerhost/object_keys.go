package computerhost

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/brokeredstore"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// ObjectKeyClient obtains per-object-set data keys. The Control Plane delivers
// a checkpoint's key only to its capturing or restoring allocation, and a boot
// seed's key only to its building allocation, a Computer booting from it or its
// preparation source.
type ObjectKeyClient interface {
	CheckpointKey(context.Context, workerapi.CheckpointKeyRequest) ([]byte, error)
	BootSeedKey(context.Context, workerapi.BootSeedKeyRequest) ([]byte, error)
}

// checkpointCipher returns the encryptor of one memory checkpoint. The caller
// closes it as soon as encryption or restore staging ends.
func (p *PreparedMachines) checkpointCipher(ctx context.Context, identity workerapi.AllocationIdentity, checkpoint string) (*CheckpointEncryptor, error) {
	if p.ObjectKeys == nil {
		return nil, errors.New("checkpoint keys are not configured")
	}
	key, err := p.ObjectKeys.CheckpointKey(ctx, workerapi.CheckpointKeyRequest{Identity: identity, CheckpointID: checkpoint})
	defer clear(key)
	if err != nil {
		return nil, err
	}
	return NewCheckpointEncryptor(key)
}

// bootSeedCipher returns the encryptor of one boot seed under the storage
// authority carried by ctx.
func (p *PreparedMachines) bootSeedCipher(ctx context.Context, build uuid.UUID) (*CheckpointEncryptor, error) {
	if p.ObjectKeys == nil {
		return nil, errors.New("boot seed keys are not configured")
	}
	scope, err := brokeredstore.Scope(ctx)
	if err != nil {
		return nil, err
	}
	key, err := p.ObjectKeys.BootSeedKey(ctx, workerapi.BootSeedKeyRequest{Scope: scope, BuildID: build.String()})
	defer clear(key)
	if err != nil {
		return nil, err
	}
	return NewCheckpointEncryptor(key)
}
