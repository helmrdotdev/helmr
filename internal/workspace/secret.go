package workspace

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/secret"
)

const MaxSecretPlacements = 64

var secretEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type SecretPlacement struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

func NormalizeSecretPlacements(input []SecretPlacement) ([]SecretPlacement, error) {
	if len(input) > MaxSecretPlacements {
		return nil, fmt.Errorf("at most %d workspace secret placements are allowed", MaxSecretPlacements)
	}
	placements := append([]SecretPlacement(nil), input...)
	envTargets := make(map[string]struct{}, len(placements))
	fileTargets := make([]string, 0, len(placements))
	for _, placement := range placements {
		if err := secret.ValidateName(placement.Name); err != nil {
			return nil, err
		}
		switch placement.Kind {
		case "env":
			if !secretEnvPattern.MatchString(placement.Target) || strings.HasPrefix(placement.Target, "HELMR_") {
				return nil, fmt.Errorf("invalid or reserved workspace secret environment target %q", placement.Target)
			}
			if _, exists := envTargets[placement.Target]; exists {
				return nil, fmt.Errorf("duplicate workspace secret environment target %q", placement.Target)
			}
			envTargets[placement.Target] = struct{}{}
		case "file":
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
	if value == "/workspace" || strings.HasPrefix(value, "/workspace/") {
		return fmt.Errorf("workspace secret file target %q overlaps the durable workspace root", value)
	}
	return nil
}
