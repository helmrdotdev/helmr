package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

// ProgramCompilePackage records a logical assertion and its actual instance.
// Evaluated config, not dependency provenance, owns compile intent.
type ProgramCompilePackage struct {
	LogicalRoot  string `json:"logicalRoot"`
	ResolvedRoot string `json:"resolvedRoot"`
}

// installedPackageRoot keeps the full prefix through the last installed slot.
// Export subdirectories (even with package.json) remain in the same instance.
func installedPackageRoot(value string) string {
	parts := strings.Split(value, "/")
	for index := len(parts) - 1; index >= 0; index-- {
		if parts[index] != "node_modules" {
			continue
		}
		if index+1 >= len(parts) || parts[index+1] == "" {
			return ""
		}
		end := index + 2
		if strings.HasPrefix(parts[index+1], "@") {
			end++
		}
		if end > len(parts) {
			return ""
		}
		return strings.Join(parts[:end], "/")
	}
	return ""
}

func selectedPackageContains(selection []ProgramCompilePackage, value string) bool {
	owner := installedPackageRoot(value)
	if owner == "" {
		return false
	}
	for _, selected := range selection {
		if owner == selected.ResolvedRoot {
			return true
		}
	}
	return false
}

func validCompileRoot(value string) bool {
	return value != "." && value != "helmr" && validateArtifactPath(value, programArtifact) == nil &&
		!hasReservedOutputSegment(value) && !strings.HasPrefix(value, "helmr/")
}

func validateProgramCompilePackages(selection []ProgramCompilePackage) error {
	if selection == nil {
		return errors.New("compile packages must be an array")
	}
	for index, selected := range selection {
		if !validCompileRoot(selected.LogicalRoot) || installedPackageRoot(selected.LogicalRoot) != selected.LogicalRoot ||
			!validCompileRoot(selected.ResolvedRoot) ||
			(hasNodeModulesComponent(selected.ResolvedRoot) && installedPackageRoot(selected.ResolvedRoot) != selected.ResolvedRoot) {
			return fmt.Errorf("compile selection package %d is not an installed package root", index)
		}
		if index > 0 && selection[index-1].LogicalRoot >= selected.LogicalRoot {
			return errors.New("compile selection packages are not in canonical order")
		}
	}
	return nil
}

func packageManifestObject(raw []byte) (map[string]json.RawMessage, error) {
	if _, err := jsoncanon.Transform(raw); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		return nil, errors.New("package.json must be a JSON object")
	}
	return document, nil
}

func verifyProgramCompilePackages(ctx context.Context, artifact *inspectedArtifact, selection []ProgramCompilePackage, configRef ProgramPathDigest) error {
	if err := validateProgramCompilePackages(selection); err != nil {
		return err
	}
	if err := verifyProgramPathDigest(ctx, artifact, configRef); err != nil {
		return err
	}
	raw, err := artifact.read(ctx, configRef.Path, maxBuildConfigBytes)
	if err != nil {
		return err
	}
	config, err := ParseBuildConfig(raw)
	if err != nil {
		return err
	}
	selectors := config.CompilePackages
	if len(selectors) != len(selection) {
		return errors.New("compile packages do not match evaluated config")
	}
	for index, selected := range selection {
		if selectors[index] != selected.LogicalRoot {
			return errors.New("compile packages do not match evaluated config")
		}
		entry, resolved, err := resolveProgramArtifactPath(artifact, selected.LogicalRoot)
		if err != nil {
			return fmt.Errorf("compile selection %q: %w", selected.LogicalRoot, err)
		}
		if entry.Kind != artifactEntryDirectory || resolved != selected.ResolvedRoot {
			return fmt.Errorf("compile selection %q does not resolve to declared package root", selected.LogicalRoot)
		}
		raw, err := artifact.read(ctx, path.Join(resolved, "package.json"), maxProgramFileSizeBytes)
		if err != nil {
			return fmt.Errorf("compile selection %q package.json: %w", selected.LogicalRoot, err)
		}
		if _, err := packageManifestObject(raw); err != nil {
			return fmt.Errorf("compile selection %q: %w", selected.LogicalRoot, err)
		}
	}
	return nil
}

func validateCompiledInputs(inputs []ProgramPathDigest, selection []ProgramCompilePackage) error {
	if inputs == nil {
		return errors.New("compiled inputs must be an array")
	}
	for index, input := range inputs {
		if err := validateProgramPathDigest(input); err != nil {
			return fmt.Errorf("compiled input %d: %w", index, err)
		}
		if hasNodeModulesComponent(input.Path) && !selectedPackageContains(selection, input.Path) {
			return fmt.Errorf("compiled input %q is not in a selected package", input.Path)
		}
		if index > 0 && inputs[index-1].Path >= input.Path {
			return errors.New("compiled inputs are not in canonical order")
		}
	}
	return nil
}
