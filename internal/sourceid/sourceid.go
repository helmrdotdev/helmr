package sourceid

import "regexp"

const Grammar = `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`

var pattern = regexp.MustCompile(Grammar)

func Valid(value string) bool {
	return pattern.MatchString(value)
}
