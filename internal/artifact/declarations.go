package artifact

import (
	"errors"
	"regexp"
	"strings"
)

var declarationModulePattern = regexp.MustCompile(`^helmr/app/entry-[0-9]+\.mjs$`)

func validateDeclarationModulePath(value string) error {
	if err := ValidatePath(value, RoleProgram); err != nil {
		return err
	}
	if !declarationModulePattern.MatchString(value) {
		return errors.New("must identify a generated declaration entry module")
	}
	return nil
}

// Only generated modules and their maps may occupy the application namespace.
func IsGeneratedProgramEntry(entry Entry) bool {
	if entry.Path == "helmr/app" || entry.Path == "helmr/app/chunks" {
		return entry.Kind == EntryDirectory
	}
	return strings.HasPrefix(entry.Path, "helmr/app/") && entry.Kind == EntryRegular &&
		(strings.HasSuffix(entry.Path, ".mjs") || strings.HasSuffix(entry.Path, ".mjs.map"))
}
