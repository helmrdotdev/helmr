// Package workerpoolname owns the Worker pool name grammar shared by the
// DB-free worker and worker supply.
package workerpoolname

import (
	"errors"
	"regexp"
)

var poolNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)

func Validate(name string) error {
	if name == "" || len(name) > 128 || !poolNamePattern.MatchString(name) {
		return errors.New("worker pool name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens")
	}
	return nil
}
