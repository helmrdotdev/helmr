package computerhost

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

const (
	controlRequestTimeout    = 5 * time.Second
	controlRequestRetryEvery = 100 * time.Millisecond
)

// errForeignSourceReceipt marks a Control Plane receipt that cannot belong to
// the physical source operation. Retrying cannot change it.
var errForeignSourceReceipt = errors.New(
	"control plane receipt does not belong to the source operation",
)

// retryControlRequest retries the physical owner's Control Plane requests
// until they succeed, fail permanently or ctx ends.
func retryControlRequest(
	ctx context.Context,
	request func(context.Context) error,
) error {
	delay := controlRequestRetryEvery
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		requestCtx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
		err := request(requestCtx)
		cancel()
		if err == nil {
			return nil
		}
		if permanentControlRequestError(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func permanentControlRequestError(err error) bool {
	if errors.Is(err, errForeignSourceReceipt) {
		return true
	}
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity,
	} {
		if httpclient.IsStatus(err, status) {
			return true
		}
	}
	return false
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
