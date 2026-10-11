package guestd

import (
	"errors"
)

func (r *computerOperationRegistry) captureSealed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.captureSealedLocked()
}

// Reserve before preparation or materialization touches the guest filesystem.
func (r *computerOperationRegistry) reserveMaterialization() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureSealedLocked() {
		return nil, errors.New("computer capture has sealed materialization")
	}
	r.materializations++
	return func() { r.mu.Lock(); r.materializations--; r.mu.Unlock() }, nil
}
