package deployment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxPackageManifestSizeBytes = 16 << 20
	maxBinCommandBytes          = 128
)

var binCommandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type packageManifest struct {
	Name             *string
	Version          *string
	Type             string
	Bins             map[string]string
	AutomaticScripts []string
	PackageManager   *string
}

func parsePackageScope(raw []byte) (packageManifest, error) {
	object, err := decodePackageManifest(raw)
	if err != nil {
		return packageManifest{}, err
	}
	manifest := packageManifest{}
	if err := parsePackageType(object, &manifest); err != nil {
		return packageManifest{}, err
	}
	return manifest, nil
}

func parseLocalPackageManifest(raw []byte, root bool) (packageManifest, error) {
	return parseOwnedPackageManifest(raw, root, true)
}

func parseRegistryPackageManifest(raw []byte) (packageManifest, error) {
	return parseOwnedPackageManifest(raw, false, false)
}

func parseOwnedPackageManifest(
	raw []byte,
	packageManagerContract bool,
	lifecycleContract bool,
) (packageManifest, error) {
	object, err := decodePackageManifest(raw)
	if err != nil {
		return packageManifest{}, err
	}
	manifest := packageManifest{Bins: map[string]string{}}
	if value, exists := object["name"]; exists {
		name, ok := value.(string)
		if !ok {
			return packageManifest{}, fmt.Errorf("package manifest name must be a string when present")
		}
		if err := validatePackageName(name); err != nil {
			return packageManifest{}, fmt.Errorf("package manifest name: %w", err)
		}
		manifest.Name = &name
	}
	if value, exists := object["version"]; exists {
		version, ok := value.(string)
		if !ok {
			return packageManifest{}, fmt.Errorf("package manifest version must be a string when present")
		}
		if err := validatePackageVersion(version); err != nil {
			return packageManifest{}, fmt.Errorf("package manifest version: %w", err)
		}
		manifest.Version = &version
	}
	if err := parsePackageType(object, &manifest); err != nil {
		return packageManifest{}, err
	}
	if value, exists := object["packageManager"]; exists && packageManagerContract {
		packageManager, ok := value.(string)
		if !ok {
			return packageManifest{}, fmt.Errorf(
				"package manifest packageManager must be a string when present",
			)
		}
		manifest.PackageManager = &packageManager
	}
	if value, exists := object["scripts"]; exists && lifecycleContract {
		scripts, ok := value.(map[string]any)
		if ok {
			for _, name := range []string{
				"preinstall",
				"install",
				"postinstall",
				"prepublish",
				"preprepare",
				"prepare",
				"postprepare",
			} {
				if _, exists := scripts[name]; exists {
					manifest.AutomaticScripts = append(manifest.AutomaticScripts, name)
				}
			}
		}
	}

	bin, exists := object["bin"]
	if !exists {
		return manifest, nil
	}
	switch value := bin.(type) {
	case string:
		if manifest.Name == nil {
			return packageManifest{}, fmt.Errorf("package manifest string bin requires name")
		}
		target, err := normalizeBinTarget(value)
		if err != nil {
			return packageManifest{}, err
		}
		name := *manifest.Name
		if separator := strings.LastIndexByte(name, '/'); separator >= 0 {
			name = name[separator+1:]
		}
		if !binCommandPattern.MatchString(name) {
			return packageManifest{}, fmt.Errorf(
				"package manifest derived bin command %q is outside the exact command domain",
				name,
			)
		}
		manifest.Bins[name] = target
	case map[string]any:
		for command, rawTarget := range value {
			if !binCommandPattern.MatchString(command) || len(command) > maxBinCommandBytes {
				return packageManifest{}, fmt.Errorf(
					"package manifest bin command %q is outside the exact command domain",
					command,
				)
			}
			target, ok := rawTarget.(string)
			if !ok {
				return packageManifest{}, fmt.Errorf(
					"package manifest bin command %q target must be a string",
					command,
				)
			}
			normalized, err := normalizeBinTarget(target)
			if err != nil {
				return packageManifest{}, fmt.Errorf(
					"package manifest bin command %q: %w",
					command,
					err,
				)
			}
			manifest.Bins[command] = normalized
		}
	default:
		return packageManifest{}, fmt.Errorf("package manifest bin must be a string or object when present")
	}
	return manifest, nil
}

func decodePackageManifest(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > maxPackageManifestSizeBytes {
		return nil, fmt.Errorf(
			"package manifest size is outside [1,%d]",
			maxPackageManifestSizeBytes,
		)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("package manifest is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder)
	if err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder, "package manifest"); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("package manifest root must be an object")
	}
	return object, nil
}

func parsePackageType(object map[string]any, manifest *packageManifest) error {
	value, exists := object["type"]
	if !exists {
		return nil
	}
	moduleType, ok := value.(string)
	if !ok || (moduleType != "module" && moduleType != "commonjs") {
		return fmt.Errorf(`package manifest type must be "module" or "commonjs" when present`)
	}
	manifest.Type = moduleType
	return nil
}

func decodeUniqueJSON(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode package manifest: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("decode package manifest object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("decode package manifest object key: expected string")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("package manifest contains duplicate object member %q", key)
			}
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			if err != nil {
				return nil, fmt.Errorf("decode package manifest object end: %w", err)
			}
			return nil, fmt.Errorf("decode package manifest object end: expected }")
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			if err != nil {
				return nil, fmt.Errorf("decode package manifest array end: %w", err)
			}
			return nil, fmt.Errorf("decode package manifest array end: expected ]")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("decode package manifest: unexpected delimiter %q", delimiter)
	}
}

func normalizeBinTarget(target string) (string, error) {
	target = strings.TrimPrefix(target, "./")
	if target == "" || !utf8.ValidString(target) || strings.HasPrefix(target, "/") ||
		strings.Contains(target, "\\") {
		return "", fmt.Errorf("bin target %q is not a confined relative POSIX path", target)
	}
	for _, character := range target {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("bin target %q contains a control character", target)
		}
	}
	for _, component := range strings.Split(target, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("bin target %q is not normalized", target)
		}
		if len(component) > maxPackagePathComponent {
			return "", fmt.Errorf(
				"bin target %q contains a component over %d bytes",
				target,
				maxPackagePathComponent,
			)
		}
	}
	return target, nil
}
