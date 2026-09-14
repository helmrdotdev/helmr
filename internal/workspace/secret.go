package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/origin"
	"github.com/helmrdotdev/helmr/internal/secret"
)

const MaxSecretPlacements = 64

var secretEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type SecretPlacement struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Target         string   `json:"target"`
	Mode           string   `json:"mode"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
}

func NormalizeSecretPlacements(input []SecretPlacement) ([]SecretPlacement, error) {
	if len(input) > MaxSecretPlacements {
		return nil, fmt.Errorf("at most %d workspace secret placements are allowed", MaxSecretPlacements)
	}
	placements := append([]SecretPlacement(nil), input...)
	envTargets := make(map[string]struct{}, len(placements))
	fileTargets := make([]string, 0, len(placements))
	originCount := 0
	for index := range placements {
		placement := &placements[index]
		if err := secret.ValidateName(placement.Name); err != nil {
			return nil, err
		}
		switch placement.Kind {
		case "env":
			if !secretEnvPattern.MatchString(placement.Target) || ReservedSecretEnv(placement.Target) {
				return nil, fmt.Errorf("invalid or reserved workspace secret environment target %q", placement.Target)
			}
			if _, exists := envTargets[placement.Target]; exists {
				return nil, fmt.Errorf("duplicate workspace secret environment target %q", placement.Target)
			}
			envTargets[placement.Target] = struct{}{}
			if placement.Mode != "raw" && placement.Mode != "protected" {
				return nil, fmt.Errorf("secret env mode must be explicit raw or protected")
			}
			if placement.Mode == "protected" {
				if len(placement.AllowedOrigins) == 0 || len(placement.AllowedOrigins) > 16 {
					return nil, fmt.Errorf("protected env requires 1 to 16 exact HTTPS origins")
				}
				placement.AllowedOrigins = slices.Clone(placement.AllowedOrigins)
				for i, value := range placement.AllowedOrigins {
					canonical, err := origin.Canonical(value)
					if err != nil {
						return nil, err
					}
					placement.AllowedOrigins[i] = canonical
				}
				slices.Sort(placement.AllowedOrigins)
				placement.AllowedOrigins = slices.Compact(placement.AllowedOrigins)
				originCount += len(placement.AllowedOrigins)
				if originCount > 256 {
					return nil, fmt.Errorf("workspace Secret origins exceed 256")
				}
			} else if len(placement.AllowedOrigins) != 0 {
				return nil, fmt.Errorf("raw env cannot have allowed origins")
			}
		case "file":
			if placement.Mode != "raw" || len(placement.AllowedOrigins) != 0 {
				return nil, fmt.Errorf("secret files are raw only")
			}
			if err := validateSecretFileTarget(placement.Target); err != nil {
				return nil, err
			}
			fileTargets = append(fileTargets, placement.Target)
		default:
			return nil, fmt.Errorf("unsupported workspace secret placement %q", placement.Kind)
		}
	}
	slices.Sort(fileTargets)
	for index, target := range fileTargets {
		if index == 0 {
			continue
		}
		previous := fileTargets[index-1]
		if target == previous || strings.HasPrefix(target, previous+"/") {
			return nil, fmt.Errorf("conflicting workspace secret file targets %q and %q", previous, target)
		}
	}
	sort.Slice(placements, func(i, j int) bool {
		if placements[i].Kind != placements[j].Kind {
			return placements[i].Kind < placements[j].Kind
		}
		if placements[i].Target != placements[j].Target {
			return placements[i].Target < placements[j].Target
		}
		return placements[i].Name < placements[j].Name
	})
	return placements, nil
}

func validateSecretFileTarget(value string) error {
	if !utf8.ValidString(value) || len(value) > 4096 || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("invalid workspace secret file target %q", value)
	}
	if !strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "/" {
		return fmt.Errorf("workspace secret file target %q must be a canonical absolute path", value)
	}
	for _, reserved := range []string{"/workspace", "/var/lib/helmr", "/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root", "/run/helmr"} {
		if value == reserved || strings.HasPrefix(value, reserved+"/") {
			return fmt.Errorf("workspace secret file target %q overlaps a reserved runtime path", value)
		}
	}
	return nil
}

func ReservedSecretEnv(name string) bool {
	if strings.HasPrefix(name, "HELMR_") || strings.HasPrefix(name, "LD_") {
		return true
	}
	switch name {
	case "NODE_OPTIONS", "NODE_PATH", "NODE_ICU_DATA", "OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES", "GCONV_PATH", "LOCPATH":
		return true
	}
	switch strings.ToUpper(name) {
	case "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA":
		return true
	}
	return false
}

func SecretPlaceholder(mode string) (string, error) {
	if mode != "protected" {
		return "", nil
	}
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "hlmr_protected_" + hex.EncodeToString(value[:]), nil
}

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
