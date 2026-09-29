package definition

import (
	"fmt"
	"regexp"
)

// DeclaredIDGrammar is the identifier grammar for declared Task, Actor and
// Sandbox IDs.
const DeclaredIDGrammar = `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`

var declaredIDPattern = regexp.MustCompile(DeclaredIDGrammar)

func ValidDeclaredID(value string) bool {
	return declaredIDPattern.MatchString(value)
}

func ValidateSandboxDeclaredID(id string) error {
	if !ValidDeclaredID(id) {
		return fmt.Errorf(
			"computer declared ID %q must match %s",
			id,
			DeclaredIDGrammar,
		)
	}
	return nil
}

var queueNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

func ValidateQueueName(name string) error {
	if !queueNamePattern.MatchString(name) {
		return fmt.Errorf("queue name %q must match %s", name, queueNamePattern.String())
	}
	return nil
}
