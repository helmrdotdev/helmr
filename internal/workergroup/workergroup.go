// Package workergroup owns worker supply: worker groups, their pools and
// hosts, worker host enrollment, epoch credentials and lifecycle, the
// /capacity/v1 scaling protocol's operations and planning, and the
// self-hosted bootstrap seed. Operations take domain inputs, own their
// transactions and locks, and return the errors declared here; callers map
// them to their transport.
package workergroup

import (
	"errors"
	"fmt"
)

var (
	ErrGroupNotFound = errors.New("worker group not found")
	ErrPoolNotFound  = errors.New("worker pool not found")
	ErrHostNotFound  = errors.New("worker host not found")
	// ErrQueuedDemand rejects a drain that requires zero queued demand while
	// the worker group's region still has queued work.
	ErrQueuedDemand = errors.New("queued demand is present")
	// ErrUnauthenticated rejects a worker host secret or epoch token that does
	// not authenticate a current worker host.
	ErrUnauthenticated = errors.New("worker authentication is required")
	// ErrInvalidEnrollmentToken rejects an enrollment that no active or
	// paused worker group's enrollment token authorizes.
	ErrInvalidEnrollmentToken = errors.New("worker enrollment token is invalid")
	// ErrObservationConflict rejects a worker host observation that does not
	// match the host's current epoch.
	ErrObservationConflict = errors.New("worker observation conflicts with this worker epoch")
)

// InputError reports a caller-supplied value that the worker supply domain
// rejects.
type InputError struct {
	message string
}

func (e InputError) Error() string {
	return e.message
}

func invalidInput(format string, args ...any) error {
	return InputError{message: fmt.Sprintf(format, args...)}
}

// ConflictError reports that the current state or a claim fence does not
// permit the requested change.
type ConflictError struct {
	message string
}

func (e ConflictError) Error() string {
	return e.message
}

func conflict(message string) error {
	return ConflictError{message: message}
}
