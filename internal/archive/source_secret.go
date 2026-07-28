package archive

import (
	"path"
	"strings"
)

func IsSourceSecretPath(name string) bool {
	base := path.Base(strings.TrimSuffix(name, "/"))
	if base == ".env" {
		return true
	}
	if !strings.HasPrefix(base, ".env.") {
		return false
	}
	for _, suffix := range []string{".example", ".sample", ".template"} {
		if strings.HasSuffix(base, suffix) {
			return false
		}
	}
	return true
}
