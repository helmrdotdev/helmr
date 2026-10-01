package controlplane

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dispatch"
)

// The triggering operation has committed. A failed capture attempt does not
// undo its receipt; the dispatcher retries it from durable state.
func (s *Server) captureDrainingComputers(ctx context.Context, hostID, instanceID uuid.UUID) {
	if _, err := dispatch.CaptureDrainingComputers(ctx, s.tx, hostID, instanceID, 50); err != nil {
		s.log.Warn("attempt draining Computer capture", "worker_host_id", hostID, "computer_instance_id", instanceID, "error", err)
	}
}
