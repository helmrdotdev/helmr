package computerhost

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
)

// waitAllocationCapacity stays inside the delivered, renewing grant. Custody
// retries must not replace that grant when health or local capacity is temporary.
func waitAllocationCapacity(ctx context.Context, admit func(context.Context) error, ledger *reservation.Ledger, key reservation.Key, request reservation.Vector, wake *allocationWake) error {
	for {
		hostWake := wake.snapshot()
		capacityWake := ledger.Changes()
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		if err := admit(ctx); err == nil {
			created, err := ledger.Reserve(key, request)
			if err == nil {
				if !created {
					return errors.New("allocation capacity is already owned")
				}
				return nil
			}
			if !errors.Is(err, reservation.ErrCapacityExceeded) {
				return err
			}
		} else if errors.Is(err, ErrCheckpointKeyUnavailable) {
			return err
		}
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-hostWake:
			timer.Stop()
		case <-capacityWake:
			timer.Stop()
		case <-timer.C:
		}

	}
}
