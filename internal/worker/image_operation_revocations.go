package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/helmrdotdev/helmr/internal/ids"
)

const maxRevokedImageOperationIDs = 10_000

var errImageOperationRevoked = errors.New("workspace image operation is revoked")

type imageOperationRegistration struct {
	cancel context.CancelFunc
}

type imageOperationRevocations struct {
	mu      sync.Mutex
	revoked map[string]struct{}
	active  map[string]*imageOperationRegistration
}

func newImageOperationRevocations() *imageOperationRevocations {
	return &imageOperationRevocations{
		revoked: make(map[string]struct{}),
		active:  make(map[string]*imageOperationRegistration),
	}
}

func (r *imageOperationRevocations) RegisterImageOperation(
	operationID string,
	cancel context.CancelFunc,
) (func(), error) {
	if _, err := ids.Parse(operationID); err != nil {
		return nil, errors.New("workspace image operation ID must be a canonical UUIDv7")
	}
	if cancel == nil {
		return nil, errors.New("workspace image operation cancellation is required")
	}
	registration := &imageOperationRegistration{cancel: cancel}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, revoked := r.revoked[operationID]; revoked {
		return nil, errImageOperationRevoked
	}
	if _, exists := r.active[operationID]; exists {
		return nil, errors.New("workspace image operation already has a live attempt")
	}
	r.active[operationID] = registration
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.active[operationID] == registration {
			delete(r.active, operationID)
		}
	}, nil
}

func (r *imageOperationRevocations) apply(operationIDs []string) error {
	if len(operationIDs) > maxRevokedImageOperationIDs {
		return fmt.Errorf(
			"revoked workspace image operation count %d exceeds %d",
			len(operationIDs),
			maxRevokedImageOperationIDs,
		)
	}
	if !slices.IsSorted(operationIDs) {
		return errors.New("revoked workspace image operation IDs are not sorted")
	}
	for index, operationID := range operationIDs {
		if index != 0 && operationIDs[index-1] == operationID {
			return errors.New("revoked workspace image operation IDs are not unique")
		}
		if _, err := ids.Parse(operationID); err != nil {
			return errors.New("revoked workspace image operation ID must be a canonical UUIDv7")
		}
	}
	callbacks := make([]context.CancelFunc, 0, len(operationIDs))
	r.mu.Lock()
	for _, operationID := range operationIDs {
		if _, exists := r.revoked[operationID]; exists {
			continue
		}
		r.revoked[operationID] = struct{}{}
		if registration := r.active[operationID]; registration != nil {
			callbacks = append(callbacks, registration.cancel)
		}
	}
	r.mu.Unlock()
	for _, cancel := range callbacks {
		cancel()
	}
	return nil
}
