package deployment

import "context"

// ImageOperationRevocations binds a logical Workspace-image operation to the
// cancellation of its current physical attempt. The Build Lease owns the
// registry; an attempt unregisters itself before its VM and provider disappear.
type ImageOperationRevocations interface {
	RegisterImageOperation(
		operationID string,
		cancel context.CancelFunc,
	) (unregister func(), err error)
}
