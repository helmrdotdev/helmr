package dispatch

import "errors"

var (
	ErrQueueUnavailable = errors.New("dispatch queue unavailable")
	ErrMessageNotFound  = errors.New("dispatch message not found")
	ErrNoCapacity       = errors.New("worker instance has no capacity")
)
