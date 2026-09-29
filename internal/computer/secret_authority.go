package computer

// SecretAuthority describes effective use of one stable scoped Secret ID.
type SecretAuthority struct {
	ID      string
	Mode    string
	Origins []string
}

func AllowsSecretAuthority(source, target []SecretAuthority) bool {
	for _, wanted := range target {
		allowed := false
		origins := map[string]bool{}
		for _, grant := range source {
			if grant.ID != wanted.ID {
				continue
			}
			if grant.Mode == "raw" {
				allowed = true
				break
			}
			if grant.Mode == "protected" {
				for _, o := range grant.Origins {
					origins[o] = true
				}
			}
		}
		if allowed {
			continue
		}
		if wanted.Mode != "protected" || len(wanted.Origins) == 0 {
			return false
		}
		for _, o := range wanted.Origins {
			if !origins[o] {
				return false
			}
		}
	}
	return true
}
