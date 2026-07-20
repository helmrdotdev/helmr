package deployment

import (
	"fmt"
	"strings"
)

func parseBunLock(raw []byte) (lockAnalysis, error) {
	root, err := decodeLockJSON(raw, true)
	if err != nil {
		return lockAnalysis{}, err
	}
	if err := requireOnlyFields(
		root,
		"Bun lockfile",
		"lockfileVersion",
		"configVersion",
		"workspaces",
		"packages",
		"trustedDependencies",
		"patchedDependencies",
		"overrides",
		"catalog",
		"catalogs",
	); err != nil {
		return lockAnalysis{}, err
	}
	version, ok := exactJSONInteger(root["lockfileVersion"])
	if !ok || (version != 0 && version != 1) {
		return lockAnalysis{}, unsupportedLockfile(
			"Bun lockfileVersion must be integer 0 or 1",
		)
	}
	if value, exists := root["configVersion"]; exists {
		configVersion, ok := exactJSONInteger(value)
		if !ok || configVersion < 0 || configVersion > 1 {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun configVersion must be integer 0 or 1",
			)
		}
	}
	if _, exists := root["patchedDependencies"]; exists {
		return lockAnalysis{}, invalidDependencySource(
			"Bun patched dependencies are unsupported",
		)
	}
	for _, field := range []string{"overrides", "catalog", "catalogs"} {
		if value, exists := root[field]; exists {
			if _, ok := value.(map[string]any); !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun %s must be an object",
					field,
				)
			}
		}
	}
	if value, exists := root["trustedDependencies"]; exists {
		if err := validateBunNameArray(value, "trustedDependencies"); err != nil {
			return lockAnalysis{}, err
		}
	}

	workspaces, ok := root["workspaces"].(map[string]any)
	if !ok {
		return lockAnalysis{}, unsupportedLockfile(
			"Bun workspaces must be an object",
		)
	}
	localPaths := make([]string, 0, len(workspaces))
	workspaceNames := make(map[string]string, len(workspaces))
	names := make(map[string]string, len(workspaces))
	for packagePath, value := range workspaces {
		workspace, ok := value.(map[string]any)
		if !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun workspace %q must be an object",
				packagePath,
			)
		}
		if err := requireOnlyFields(
			workspace,
			fmt.Sprintf("Bun workspace %q", packagePath),
			"name",
			"version",
			"bin",
			"binDir",
			"dependencies",
			"devDependencies",
			"optionalDependencies",
			"peerDependencies",
			"optionalPeers",
		); err != nil {
			return lockAnalysis{}, err
		}
		normalized := packagePath
		if normalized == "" {
			normalized = "."
		}
		if normalized != "." {
			if err := validatePackagePath(normalized, programMountPath, true); err != nil {
				return lockAnalysis{}, invalidDependencySource(
					"Bun workspace path %q: %v",
					packagePath,
					err,
				)
			}
		}
		name := ""
		if rawName, exists := workspace["name"]; exists {
			name, ok = rawName.(string)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun workspace %q name must be a string",
					packagePath,
				)
			}
			if err := validatePackageName(name); err != nil {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun workspace %q name: %v",
					packagePath,
					err,
				)
			}
		} else if normalized != "." {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun workspace %q requires a name",
				packagePath,
			)
		}
		if name != "" {
			if previous, exists := names[name]; exists {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun workspaces %q and %q have duplicate name %q",
					previous,
					normalized,
					name,
				)
			}
			names[name] = normalized
		}
		if rawVersion, exists := workspace["version"]; exists {
			value, ok := rawVersion.(string)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun workspace %q version must be a string",
					packagePath,
				)
			}
			if err := validatePackageVersion(value); err != nil {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun workspace %q version: %v",
					packagePath,
					err,
				)
			}
		}
		for _, field := range []string{
			"dependencies",
			"devDependencies",
			"optionalDependencies",
			"peerDependencies",
		} {
			if err := validateBunDependencyObject(
				workspace[field],
				fmt.Sprintf("Bun workspace %q %s", packagePath, field),
			); err != nil {
				return lockAnalysis{}, err
			}
		}
		if value, exists := workspace["optionalPeers"]; exists {
			if err := validateBunNameArray(value, "optionalPeers"); err != nil {
				return lockAnalysis{}, err
			}
		}
		localPaths = append(localPaths, normalized)
		workspaceNames[normalized] = name
	}

	packages := map[string]any{}
	if value, exists := root["packages"]; exists {
		packages, ok = value.(map[string]any)
		if !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun packages must be an object",
			)
		}
	}
	confirmed := make(map[string]struct{}, len(workspaceNames))
	pins := make([]RegistryPin, 0, len(packages))
	for packageKey, rawTuple := range packages {
		if err := validateBunPackageKey(packageKey); err != nil {
			return lockAnalysis{}, invalidDependencySource(
				"Bun package path %q: %v",
				packageKey,
				err,
			)
		}
		tuple, ok := rawTuple.([]any)
		if !ok || len(tuple) == 0 {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun package %q must be a non-empty tuple",
				packageKey,
			)
		}
		resolution, ok := tuple[0].(string)
		if !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun package %q resolution must be a string",
				packageKey,
			)
		}
		name, source, ok := splitBunResolution(resolution)
		if !ok {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun package %q has unknown resolution %q",
				packageKey,
				resolution,
			)
		}
		switch {
		case strings.HasPrefix(source, "workspace:"):
			switch version {
			case 0:
				if len(tuple) != 2 {
					return lockAnalysis{}, unsupportedLockfile(
						"Bun v0 workspace package %q has an unknown tuple shape",
						packageKey,
					)
				}
				info, ok := tuple[1].(map[string]any)
				if !ok {
					return lockAnalysis{}, unsupportedLockfile(
						"Bun v0 workspace package %q metadata must be an object",
						packageKey,
					)
				}
				if err := validateBunPackageInfo(packageKey, info); err != nil {
					return lockAnalysis{}, err
				}
			case 1:
				if len(tuple) != 1 {
					return lockAnalysis{}, unsupportedLockfile(
						"Bun v1 workspace package %q has an unknown tuple shape",
						packageKey,
					)
				}
			}
			workspacePath := strings.TrimPrefix(source, "workspace:")
			expected, exists := workspaceNames[workspacePath]
			if !exists || workspacePath == "." || workspacePath == "" {
				return lockAnalysis{}, invalidDependencySource(
					"Bun package %q references unknown workspace %q",
					packageKey,
					workspacePath,
				)
			}
			if name != expected {
				return lockAnalysis{}, invalidDependencySource(
					"Bun package %q workspace identity does not match %q",
					packageKey,
					workspacePath,
				)
			}
			confirmed[workspacePath] = struct{}{}
		case source == "root:":
			if len(tuple) != 2 {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun root package %q has an unknown tuple shape",
					packageKey,
				)
			}
			info, ok := tuple[1].(map[string]any)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun root package %q metadata must be an object",
					packageKey,
				)
			}
			if err := validateBunPackageInfo(packageKey, info); err != nil {
				return lockAnalysis{}, err
			}
			if rootName := workspaceNames["."]; rootName != "" && name != rootName {
				return lockAnalysis{}, invalidDependencySource(
					"Bun root package %q has a divergent identity",
					packageKey,
				)
			}
		case prohibitedDependencySpecifier(source):
			return lockAnalysis{}, invalidDependencySource(
				"Bun package %q uses unsupported source %q",
				packageKey,
				source,
			)
		default:
			if len(tuple) != 4 {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun registry package %q has an unknown tuple shape",
					packageKey,
				)
			}
			registry, ok := tuple[1].(string)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun registry package %q origin must be a string",
					packageKey,
				)
			}
			if registry != "" {
				return lockAnalysis{}, invalidDependencySource(
					"Bun registry package %q uses a non-public origin",
					packageKey,
				)
			}
			info, ok := tuple[2].(map[string]any)
			if !ok {
				return lockAnalysis{}, unsupportedLockfile(
					"Bun registry package %q metadata must be an object",
					packageKey,
				)
			}
			if err := validateBunPackageInfo(packageKey, info); err != nil {
				return lockAnalysis{}, err
			}
			integrity, ok := tuple[3].(string)
			if !ok {
				return lockAnalysis{}, invalidDependencySource(
					"Bun registry package %q has no integrity",
					packageKey,
				)
			}
			pins = append(pins, RegistryPin{
				Name:      name,
				Version:   source,
				Integrity: integrity,
			})
		}
	}
	for workspacePath := range workspaceNames {
		if workspacePath == "." {
			continue
		}
		if _, exists := confirmed[workspacePath]; !exists {
			return lockAnalysis{}, unsupportedLockfile(
				"Bun workspace %q has no package tuple",
				workspacePath,
			)
		}
	}
	return lockAnalysis{localPaths: localPaths, registryPins: pins}, nil
}

func validateBunPackageKey(value string) error {
	if err := validatePackagePath(value, programMountPath, false); err != nil {
		return err
	}
	components := strings.Split(value, "/")
	for position := 0; position < len(components); {
		name := components[position]
		position++
		if strings.HasPrefix(name, "@") {
			if position >= len(components) {
				return fmt.Errorf("scope %q has no package name", name)
			}
			name += "/" + components[position]
			position++
		}
		if err := validatePackageName(name); err != nil {
			return err
		}
	}
	return nil
}

func splitBunResolution(value string) (string, string, bool) {
	separator := strings.LastIndexByte(value, '@')
	if separator <= 0 || separator == len(value)-1 {
		return "", "", false
	}
	name := value[:separator]
	source := value[separator+1:]
	if err := validatePackageName(name); err != nil {
		return "", "", false
	}
	return name, source, true
}

func validateBunPackageInfo(packageKey string, object map[string]any) error {
	if err := requireOnlyFields(
		object,
		fmt.Sprintf("Bun package %q metadata", packageKey),
		"dependencies",
		"devDependencies",
		"optionalDependencies",
		"peerDependencies",
		"optionalPeers",
		"bundled",
		"os",
		"cpu",
		"libc",
		"bin",
		"binDir",
	); err != nil {
		return err
	}
	for _, field := range []string{
		"dependencies",
		"devDependencies",
		"optionalDependencies",
		"peerDependencies",
	} {
		if err := validateBunDependencyObject(
			object[field],
			fmt.Sprintf("Bun package %q %s", packageKey, field),
		); err != nil {
			return err
		}
	}
	if value, exists := object["optionalPeers"]; exists {
		if err := validateBunNameArray(value, "optionalPeers"); err != nil {
			return err
		}
	}
	if value, exists := object["bundled"]; exists {
		if _, ok := value.(bool); !ok {
			return unsupportedLockfile(
				"Bun package %q bundled must be a boolean",
				packageKey,
			)
		}
	}
	return nil
}

func validateBunDependencyObject(value any, label string) error {
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

func validateBunNameArray(value any, label string) error {
	values, ok := value.([]any)
	if !ok || len(values) > maxBunDependencyNames {
		return unsupportedLockfile(
			"Bun %s must be an array of at most %d package names",
			label,
			maxBunDependencyNames,
		)
	}
	seen := make(map[string]struct{}, len(values))
	for _, item := range values {
		name, ok := item.(string)
		if !ok {
			return unsupportedLockfile(
				"Bun %s must contain only package names",
				label,
			)
		}
		if err := validatePackageName(name); err != nil {
			return unsupportedLockfile("Bun %s: %v", label, err)
		}
		if _, exists := seen[name]; exists {
			return unsupportedLockfile(
				"Bun %s contains duplicate package %q",
				label,
				name,
			)
		}
		seen[name] = struct{}{}
	}
	return nil
}
