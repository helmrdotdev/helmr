package definition

import "regexp"

// DeclaredIDGrammar is the identifier grammar for declared Agent and Computer IDs.
const DeclaredIDGrammar = `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`

var declaredIDPattern = regexp.MustCompile(DeclaredIDGrammar)

func ValidDeclaredID(value string) bool {
	return declaredIDPattern.MatchString(value)
}
