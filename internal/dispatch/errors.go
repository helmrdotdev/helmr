package dispatch

import "errors"

var (
	ErrQueueUnavailable = errors.New("dispatch queue unavailable")
	ErrMessageNotFound  = errors.New("dispatch message not found")
	ErrLeaseExpired     = errors.New("queue lease expired")
	ErrLeaseConflict    = errors.New("queue lease conflict")
	ErrNoCapacity       = errors.New("worker instance has no capacity")
)
