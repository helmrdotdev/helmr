package workergroup

import (
	"errors"
	"regexp"
)

var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)

func ValidateName(name string) error {
	if name == "" || len(name) > 128 || !namePattern.MatchString(name) {
		return errors.New("worker group name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens")
	}
	return nil
}

func ValidatePoolName(name string) error {
	if name == "" || len(name) > 128 || !namePattern.MatchString(name) {
		return errors.New("worker pool name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens")
	}
	return nil
}

func ValidateRoles(allowsRun, allowsBuild bool) error {
	if !allowsRun && !allowsBuild {
		return errors.New("worker group must allow run, build, or both")
	}
	return nil
}
