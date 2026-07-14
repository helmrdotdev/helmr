package control

import "context"

func (s *Server) reconcilePreparedRuntimeSupplyAsync(ctx context.Context, reason string) {
	if s.preparedRuntimeSupply == nil {
		return
	}
	go func() {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), preparedRuntimeSupplyReconcileTimeout)
		defer cancel()
		if err := s.preparedRuntimeSupply.Reconcile(reconcileCtx); err != nil && s.log != nil {
			s.log.Warn("prepared runtime supply reconcile failed", "reason", reason, "error", err)
		}
	}()
}
