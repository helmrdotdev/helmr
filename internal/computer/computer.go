// Package computer owns external Computer creation, reads, member discovery and
// deletion, including Secret placement validation and retry receipts. The Agent
// domain owns allocation, preparation, Session membership and saving lifecycles.
package computer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	// ErrNotFound reports that no Computer matches the address in its scope.
	ErrNotFound = errors.New("computer was not found")
	// ErrBusy reports that the Computer's current members or revision do not
	// permit the change; the caller may retry.
	ErrBusy = errors.New("computer is busy")
	// ErrDeleting reports that the Computer is being deleted and admits no
	// new work.
	ErrDeleting = errors.New("computer is deleting")
	// ErrRecoveryRequired reports that the Computer failed a capture or
	// recovery, or is in recovery, and admits no new work until it is
	// recovered.
	ErrRecoveryRequired = errors.New("computer requires recovery")
	// ErrPreparationExhausted reports that the Computer reached its
	// preparation limit and admits no new work.
	ErrPreparationExhausted = errors.New("computer preparation limit reached")
	// ErrNotDeployed reports that the addressed Computer declaration is not in
	// the deployment the creation resolves.
	ErrNotDeployed = errors.New("computer declaration is not deployed")
	// ErrSecretUnavailable reports a Secret that is not active, or a binding
	// that exceeds the source Computer's secret authority.
	ErrSecretUnavailable = errors.New("computer secret is unavailable")
	// ErrReceiptInvalid reports a stored idempotency receipt this owner did
	// not write.
	ErrReceiptInvalid = errors.New("computer idempotency receipt is invalid")
)

// InputError reports a caller-supplied value that the Computer domain
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

// KeyConflictError reports that another Computer in the environment holds
// the requested key.
type KeyConflictError struct {
	Key string
}

func (e KeyConflictError) Error() string {
	return fmt.Sprintf("computer key %q is already in use", e.Key)
}

// ValidateKey checks a caller-chosen Computer key. A nil key is valid.
func ValidateKey(value *string) error {
	if value == nil {
		return nil
	}
	if !utf8.ValidString(*value) || len(*value) < 1 || len(*value) > 512 {
		return InputError{message: "computer key must contain 1 to 512 UTF-8 bytes"}
	}
	if strings.TrimSpace(*value) != *value {
		return InputError{message: "computer key cannot begin or end with whitespace"}
	}
	return nil
}
