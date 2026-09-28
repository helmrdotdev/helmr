// Package secretname validates the public identifier grammar shared by Secret
// storage and Computer bindings without depending on persistence.
package secretname

import (
	"fmt"
	"regexp"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func Validate(name string) error {
	if !pattern.MatchString(name) {
		return fmt.Errorf("secret name %q must match %s", name, pattern.String())
	}
	return nil
}
