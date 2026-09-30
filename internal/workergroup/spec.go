package workergroup

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/workerpoolname"
)

// ValidateName applies the Worker pool name grammar to Worker group names.
func ValidateName(name string) error {
	if workerpoolname.Validate(name) != nil {
		return errors.New("worker group name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens")
	}
	return nil
}
