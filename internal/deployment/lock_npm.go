package deployment

import (
	"fmt"
	"net/url"
	"strings"
)

func parseNPMLock(raw []byte) (lockAnalysis, error) {
	root, err := decodeLockJSON(raw, false)
	if err != nil {
		return lockAnalysis{}, err
	}
	if err := requireOnlyFields(
		root,
		"npm lockfile",
		"name",
		"version",
		"lockfileVersion",
		"requires",
		"packages",
	); err != nil {
		return lockAnalysis{}, err
	}
	version, ok := exactJSONInteger(root["lockfileVersion"])
	if !ok || version != 3 {
		return lockAnalysis{}, unsupportedLockfile(
			"npm lockfileVersion must be integer 3",
		)
	}
	for _, field := range []string{"name", "version"} {
		if value, exists := root[field]; exists {
			if _, ok := value.(string); !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"npm root %s must be a string",
					field,
				)
			}
		}
	}
	if value, exists := root["requires"]; exists {
		if _, ok := value.(bool); !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"npm root requires must be a boolean",
			)
		}
	}
	packages, ok := root["packages"].(map[string]any)
	if !ok {
		return lockAnalysis{}, unsupportedLockfile(
			"npm packages must be an object",
		)
	}

	localPaths := make([]string, 0)
	localSet := make(map[string]struct{})
	links := make([]string, 0)
	pins := make([]RegistryPin, 0, len(packages))
	for packagePath, rawEntry := range packages {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"npm package %q must be an object",
				packagePath,
			)
		}
		if err := requireOnlyFields(
			entry,
			fmt.Sprintf("npm package %q", packagePath),
			"name",
			"version",
			"resolved",
			"integrity",
			"link",
			"acceptDependencies",
			"bin",
			"bundleDependencies",
			"bundled",
			"cpu",
			"dependencies",
			"deprecated",
			"dev",
			"devDependencies",
			"devOptional",
			"engines",
			"extraneous",
			"funding",
			"hasInstallScript",
			"hasShrinkwrap",
			"inBundle",
			"license",
			"libc",
			"optional",
			"optionalDependencies",
			"os",
			"peer",
			"peerDependencies",
			"peerDependenciesMeta",
			"requires",
			"workspaces",
		); err != nil {
			return lockAnalysis{}, err
		}
		for _, field := range []string{
			"dependencies",
			"devDependencies",
			"optionalDependencies",
			"peerDependencies",
		} {
			if err := validateNPMDependencyObject(
				entry[field],
				fmt.Sprintf("npm package %q %s", packagePath, field),
			); err != nil {
				return lockAnalysis{}, err
			}
		}

		installName, registry, err := classifyNPMPackagePath(packagePath)
		if err != nil {
			return lockAnalysis{}, invalidDependencySource(
				"npm package path %q: %v",
				packagePath,
				err,
			)
		}
		if !registry {
			normalized := packagePath
			if normalized == "" {
				normalized = "."
			}
			if normalized != "." {
				if err := validatePackagePath(
					normalized,
					programMountPath,
					true,
				); err != nil {
					return lockAnalysis{}, invalidDependencySource(
						"npm workspace path %q: %v",
						packagePath,
						err,
					)
				}
			}
			if _, exists := entry["link"]; exists {
				return lockAnalysis{}, unsupportedLockfile(
					"npm local package %q has link metadata",
					packagePath,
				)
			}
			localSet[normalized] = struct{}{}
			localPaths = append(localPaths, normalized)
			continue
		}

		if rawLink, exists := entry["link"]; exists {
			link, ok := rawLink.(bool)
			if !ok || !link {
				return lockAnalysis{}, unsupportedLockfile(
					"npm package %q link must be true",
					packagePath,
				)
			}
			if err := requireOnlyFields(
				entry,
				fmt.Sprintf("npm link %q", packagePath),
				"resolved",
				"link",
			); err != nil {
				return lockAnalysis{}, err
			}
			resolved, ok := entry["resolved"].(string)
			if !ok || resolved == "" {
				return lockAnalysis{}, unsupportedLockfile(
					"npm link %q requires a resolved path",
					packagePath,
				)
			}
			if err := validatePackagePath(
				resolved,
				programMountPath,
				true,
			); err != nil {
				return lockAnalysis{}, invalidDependencySource(
					"npm link %q target %q: %v",
					packagePath,
					resolved,
					err,
				)
			}
			links = append(links, resolved)
			continue
		}

		name := installName
		if rawName, exists := entry["name"]; exists {
			name, ok = rawName.(string)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"npm package %q name must be a string",
					packagePath,
				)
			}
		}
		if err := validatePackageName(name); err != nil {
			return lockAnalysis{}, unsupportedLockfile(
				"npm package %q name: %v",
				packagePath,
				err,
			)
		}
		packageVersion, ok := entry["version"].(string)
		if !ok || packageVersion == "" {
			return lockAnalysis{}, invalidDependencySource(
				"npm registry package %q has no exact version",
				packagePath,
			)
		}
		resolved, ok := entry["resolved"].(string)
		if !ok || resolved == "" {
			return lockAnalysis{}, invalidDependencySource(
				"npm registry package %q has no public-registry source",
				packagePath,
			)
		}
		if prohibitedDependencySpecifier(resolved) &&
			!strings.HasPrefix(strings.ToLower(resolved), "https:") {
			return lockAnalysis{}, invalidDependencySource(
				"npm registry package %q uses unsupported source %q",
				packagePath,
				resolved,
			)
		}
		if err := validateNPMRegistryURL(resolved); err != nil {
			return lockAnalysis{}, invalidDependencySource(
				"npm registry package %q source: %v",
				packagePath,
				err,
			)
		}
		integrity, ok := entry["integrity"].(string)
		if !ok || integrity == "" {
			return lockAnalysis{}, invalidDependencySource(
				"npm registry package %q has no integrity",
				packagePath,
			)
		}
		pins = append(pins, RegistryPin{
			Name:      name,
			Version:   packageVersion,
			Integrity: integrity,
		})
	}
	if _, exists := localSet["."]; !exists {
		return lockAnalysis{}, unsupportedLockfile(
			"npm packages has no root entry",
		)
	}
	confirmed := make(map[string]struct{}, len(links))
	for _, target := range links {
		if _, exists := localSet[target]; !exists {
			return lockAnalysis{}, invalidDependencySource(
				"npm link references unknown local package %q",
				target,
			)
		}
		confirmed[target] = struct{}{}
	}
	for packagePath := range localSet {
		if packagePath == "." {
			continue
		}
		if _, exists := confirmed[packagePath]; !exists {
			return lockAnalysis{}, unsupportedLockfile(
				"npm local package %q has no workspace link",
				packagePath,
			)
		}
	}
	return lockAnalysis{localPaths: localPaths, registryPins: pins}, nil
}

func classifyNPMPackagePath(packagePath string) (string, bool, error) {
	if packagePath == "" {
		return "", false, nil
	}
	if err := validatePackagePath(packagePath, programMountPath, false); err != nil {
		return "", false, err
	}
	components := strings.Split(packagePath, "/")
	first := -1
	for i, component := range components {
		if component == "node_modules" {
			first = i
			break
		}
	}
	if first < 0 {
		return "", false, nil
	}
	if first > 0 {
		prefix := strings.Join(components[:first], "/")
		if err := validatePackagePath(prefix, programMountPath, true); err != nil {
			return "", false, err
		}
	}
	installName := ""
	for position := first; position < len(components); {
		if components[position] != "node_modules" {
			return "", false, fmt.Errorf(
				"component %q does not follow a package location",
				components[position],
			)
		}
		position++
		if position >= len(components) {
			return "", false, fmt.Errorf("node_modules has no package name")
		}
		installName = components[position]
		position++
		if strings.HasPrefix(installName, "@") {
			if position >= len(components) {
				return "", false, fmt.Errorf("scope %q has no package name", installName)
			}
			installName += "/" + components[position]
			position++
		}
		if err := validatePackageName(installName); err != nil {
			return "", false, err
		}
		if position < len(components) && components[position] != "node_modules" {
			return "", false, fmt.Errorf(
				"package %q has trailing component %q",
				installName,
				components[position],
			)
		}
	}
	return installName, true, nil
}

func validateNPMDependencyObject(value any, label string) error {
	if value == nil {
		return nil
	}
	dependencies, ok := value.(map[string]any)
	if !ok {
		return unsupportedLockfile("%s must be an object", label)
	}
	for name, rawSpec := range dependencies {
		if err := validatePackageName(name); err != nil {
			return unsupportedLockfile("%s name: %v", label, err)
		}
		spec, ok := rawSpec.(string)
		if !ok {
			return unsupportedLockfile("%s %q must be a string", label, name)
		}
		if prohibitedDependencySpecifier(spec) {
			return invalidDependencySource(
				"%s %q uses unsupported source %q",
				label,
				name,
				spec,
			)
		}
	}
	return nil
}

func validateNPMRegistryURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil {
		return fmt.Errorf("resolved URL is invalid: %v", err)
	}
	if parsed.Scheme != "https" ||
		parsed.Host != "registry.npmjs.org" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return fmt.Errorf("resolved URL is outside the public npm registry")
	}
	if !strings.HasPrefix(parsed.Path, "/") ||
		!strings.Contains(parsed.Path, "/-/") ||
		!strings.HasSuffix(parsed.Path, ".tgz") {
		return fmt.Errorf("resolved URL is not a registry tarball")
	}
	return nil
}
