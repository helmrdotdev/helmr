package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// ProgramCompileSelection binds static compile intent to the installed tree.
// Roots identify actual instances, never a package's acquisition or source.
type ProgramCompileSelection struct {
	PackageJSONDigest string                  `json:"packageJsonDigest"`
	Packages          []ProgramCompilePackage `json:"packages"`
}

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

func selectedPackageContains(selection ProgramCompileSelection, value string) bool {
	owner := installedPackageRoot(value)
	if owner == "" {
		return false
	}
	for _, selected := range selection.Packages {
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

func validateProgramCompileSelection(selection ProgramCompileSelection) error {
	if !sha256DigestPattern.MatchString(selection.PackageJSONDigest) || selection.Packages == nil {
		return errors.New("compile selection requires package.json digest and packages array")
	}
	for index, selected := range selection.Packages {
		if !validCompileRoot(selected.LogicalRoot) || installedPackageRoot(selected.LogicalRoot) != selected.LogicalRoot ||
			!validCompileRoot(selected.ResolvedRoot) ||
			(hasNodeModulesComponent(selected.ResolvedRoot) && installedPackageRoot(selected.ResolvedRoot) != selected.ResolvedRoot) {
			return fmt.Errorf("compile selection package %d is not an installed package root", index)
		}
		if index > 0 && selection.Packages[index-1].LogicalRoot >= selected.LogicalRoot {
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

func manifestCompilePackages(raw []byte) ([]string, error) {
	document, err := packageManifestObject(raw)
	if err != nil {
		return nil, err
	}
	selectors := []string{}
	if helmr, exists := document["helmr"]; exists {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(helmr, &options); err != nil || options == nil {
			return nil, errors.New("package.json helmr must be an object containing only compilePackages")
		}
		for key := range options {
			if key != "compilePackages" {
				return nil, fmt.Errorf("package.json helmr has unknown field %q", key)
			}
		}
		if value, exists := options["compilePackages"]; exists {
			// JSON null is not an empty collection (including null string elements).
			var values []json.RawMessage
			if err := json.Unmarshal(value, &values); err != nil || values == nil {
				return nil, errors.New("package.json helmr.compilePackages must be a string array")
			}
			for _, value := range values {
				var selector string
				if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &selector) != nil {
					return nil, errors.New("package.json helmr.compilePackages must be a string array")
				}
				selectors = append(selectors, selector)
			}
		}
	}
	slices.Sort(selectors)
	for index, selector := range selectors {
		if !validCompileRoot(selector) || installedPackageRoot(selector) != selector ||
			(index > 0 && selectors[index-1] == selector) {
			return nil, fmt.Errorf("package.json helmr.compilePackages has invalid or duplicate selector %q", selector)
		}
	}
	return selectors, nil
}

func verifyProgramCompileSelection(ctx context.Context, artifact *inspectedArtifact, selection ProgramCompileSelection) error {
	if err := validateProgramCompileSelection(selection); err != nil {
		return err
	}
	raw, err := artifact.read(ctx, "package.json", maxProgramFileSizeBytes)
	if err != nil {
		return err
	}
	if sha256sum.DigestBytes(raw) != selection.PackageJSONDigest {
		return errors.New("compile selection package.json digest does not match authority")
	}
	selectors, err := manifestCompilePackages(raw)
	if err != nil {
		return err
	}
	if len(selectors) != len(selection.Packages) {
		return errors.New("compile selection does not match package.json helmr.compilePackages")
	}
	for index, selected := range selection.Packages {
		if selectors[index] != selected.LogicalRoot {
			return errors.New("compile selection does not match package.json helmr.compilePackages")
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

func validateCompiledInputs(inputs []ProgramPathDigest, selection ProgramCompileSelection) error {
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
