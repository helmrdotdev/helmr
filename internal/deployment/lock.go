package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxLocalManifestBytes int64 = 256 << 20
	maxBunDependencyNames       = 1_024
)

var (
	ErrLockfileUnsupported     = errors.New("unsupported lockfile format")
	ErrDependencySourceInvalid = errors.New("invalid dependency source")
	ErrDependencyOutputInvalid = errors.New("invalid dependency output")
)

type RegistryPin struct {
	Name      string
	Version   string
	Integrity string
}

type SourceManifest struct {
	PackagePath    string
	Bytes          []byte
	ManifestDigest string
	Name           *string
	Version        *string
}

type DependencySource struct {
	PackageManager PackageManager
	Lockfile       DependencyLockfile
	LockfileBytes  []byte
	LocalManifests LocalManifests
	ManifestFiles  []SourceManifest
	RegistryPins   []RegistryPin
}

type lockAnalysis struct {
	localPaths   []string
	registryPins []RegistryPin
}

func InspectDependencySource(projectRoot string, manager PackageManager) (DependencySource, error) {
	if err := validateManagerPackage(manager); err != nil {
		return DependencySource{}, invalidDependencySource("%v", err)
	}
	root, err := os.OpenRoot(projectRoot)
	if err != nil {
		return DependencySource{}, invalidDependencySource(
			"open project root: %v",
			err,
		)
	}
	defer root.Close()

	lockName := "bun.lock"
	if manager.Name == PackageManagerNPM {
		lockName = "package-lock.json"
	}
	if err := validateDependencySourceLayout(root, lockName); err != nil {
		return DependencySource{}, err
	}

	lockBytes, err := readDependencyFile(root, lockName, maxLockfileBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DependencySource{}, unsupportedLockfile(
				"selected lockfile %q is missing",
				lockName,
			)
		}
		return DependencySource{}, invalidDependencySource(
			"read selected lockfile %q: %v",
			lockName,
			err,
		)
	}
	analysis, err := parseDependencyLockfile(manager.Name, lockBytes)
	if err != nil {
		return DependencySource{}, err
	}
	localPaths, err := canonicalLocalPaths(analysis.localPaths)
	if err != nil {
		return DependencySource{}, err
	}
	registryPins, err := canonicalRegistryPins(analysis.registryPins)
	if err != nil {
		return DependencySource{}, err
	}

	manifests := LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries:       make([]LocalManifestEntry, 0, len(localPaths)),
	}
	files := make([]SourceManifest, 0, len(localPaths))
	var totalBytes int64
	for _, packagePath := range localPaths {
		manifestPath := "package.json"
		if packagePath != "." {
			manifestPath = path.Join(packagePath, "package.json")
		}
		raw, err := readDependencyFile(root, manifestPath, maxPackageManifestSizeBytes)
		if err != nil {
			return DependencySource{}, invalidDependencySource(
				"read local package %q manifest: %v",
				packagePath,
				err,
			)
		}
		totalBytes += int64(len(raw))
		if totalBytes > maxLocalManifestBytes {
			return DependencySource{}, invalidDependencySource(
				"local package manifests exceed %d bytes",
				maxLocalManifestBytes,
			)
		}
		manifest, err := validateSourceManifest(
			raw,
			packagePath == ".",
			manager,
		)
		if err != nil {
			return DependencySource{}, invalidDependencySource(
				"local package %q manifest: %v",
				packagePath,
				err,
			)
		}
		digest := digestBytes(raw)
		manifests.Entries = append(manifests.Entries, LocalManifestEntry{
			ManifestDigest: digest,
			Path:           packagePath,
		})
		files = append(files, SourceManifest{
			PackagePath:    packagePath,
			Bytes:          append([]byte(nil), raw...),
			ManifestDigest: digest,
			Name:           copyOptionalString(manifest.Name),
			Version:        copyOptionalString(manifest.Version),
		})
	}
	if err := ValidateLocalManifests(manifests); err != nil {
		return DependencySource{}, invalidDependencySource("%v", err)
	}

	return DependencySource{
		PackageManager: manager,
		Lockfile: DependencyLockfile{
			Name:   lockName,
			Digest: digestBytes(lockBytes),
		},
		LockfileBytes:  append([]byte(nil), lockBytes...),
		LocalManifests: manifests,
		ManifestFiles:  files,
		RegistryPins:   registryPins,
	}, nil
}

func ValidateDependencyGraph(source DependencySource, graph PackageGraph) error {
	if err := validateDependencySource(source); err != nil {
		return invalidDependencyOutput("preflight source is inconsistent: %v", err)
	}
	if err := ValidatePackageGraph(graph); err != nil {
		return invalidDependencyOutput("%v", err)
	}
	if len(source.ManifestFiles) != len(source.LocalManifests.Entries) {
		return invalidDependencyOutput("preflight manifest sets are inconsistent")
	}
	if len(graph.LocalPackages) != len(source.LocalManifests.Entries) {
		return invalidDependencyOutput(
			"local package count = %d, want %d",
			len(graph.LocalPackages),
			len(source.LocalManifests.Entries),
		)
	}
	for i, local := range graph.LocalPackages {
		manifest := source.LocalManifests.Entries[i]
		sourceFile := source.ManifestFiles[i]
		if sourceFile.PackagePath != manifest.Path ||
			sourceFile.ManifestDigest != manifest.ManifestDigest {
			return invalidDependencyOutput(
				"preflight local package %d is inconsistent",
				i,
			)
		}
		if local.Path != manifest.Path ||
			local.ManifestDigest != manifest.ManifestDigest ||
			!equalOptionalString(local.Name, sourceFile.Name) ||
			!equalOptionalString(local.Version, sourceFile.Version) {
			return invalidDependencyOutput(
				"local package %d does not match the submitted manifest set",
				i,
			)
		}
	}
	pins := make(map[string]struct{}, len(source.RegistryPins))
	for _, pin := range source.RegistryPins {
		pins[registryPinKey(pin)] = struct{}{}
	}
	for _, pkg := range graph.RegistryPackages {
		pin := RegistryPin{
			Name:      pkg.Name,
			Version:   pkg.Version,
			Integrity: pkg.Integrity,
		}
		if _, ok := pins[registryPinKey(pin)]; !ok {
			return invalidDependencyOutput(
				"registry package %q is outside the lockfile allow-set",
				pkg.InstallPath,
			)
		}
	}
	return nil
}

func validateDependencySource(source DependencySource) error {
	if err := validateManagerPackage(source.PackageManager); err != nil {
		return invalidDependencySource("%v", err)
	}
	wantLock := "bun.lock"
	if source.PackageManager.Name == PackageManagerNPM {
		wantLock = "package-lock.json"
	}
	if source.Lockfile.Name != wantLock ||
		source.Lockfile.Digest != digestBytes(source.LockfileBytes) ||
		len(source.LockfileBytes) == 0 ||
		int64(len(source.LockfileBytes)) > maxLockfileBytes {
		return invalidDependencySource("selected lockfile identity is inconsistent")
	}
	analysis, err := parseDependencyLockfile(
		source.PackageManager.Name,
		source.LockfileBytes,
	)
	if err != nil {
		return invalidDependencySource("selected lockfile: %v", err)
	}
	paths, err := canonicalLocalPaths(analysis.localPaths)
	if err != nil {
		return invalidDependencySource("selected lockfile local packages: %v", err)
	}
	pins, err := canonicalRegistryPins(analysis.registryPins)
	if err != nil {
		return invalidDependencySource("selected lockfile registry pins: %v", err)
	}
	if len(paths) != len(source.ManifestFiles) ||
		len(paths) != len(source.LocalManifests.Entries) ||
		len(pins) != len(source.RegistryPins) {
		return invalidDependencySource("preflight sets are inconsistent")
	}
	if err := ValidateLocalManifests(source.LocalManifests); err != nil {
		return invalidDependencySource("local manifests: %v", err)
	}
	var totalBytes int64
	for i, packagePath := range paths {
		file := source.ManifestFiles[i]
		entry := source.LocalManifests.Entries[i]
		if file.PackagePath != packagePath ||
			entry.Path != packagePath ||
			file.ManifestDigest != digestBytes(file.Bytes) ||
			entry.ManifestDigest != file.ManifestDigest ||
			len(file.Bytes) == 0 ||
			int64(len(file.Bytes)) > maxPackageManifestSizeBytes {
			return invalidDependencySource(
				"local package %q identity is inconsistent",
				packagePath,
			)
		}
		totalBytes += int64(len(file.Bytes))
		if totalBytes > maxLocalManifestBytes {
			return invalidDependencySource(
				"local package manifests exceed %d bytes",
				maxLocalManifestBytes,
			)
		}
		manifest, err := validateSourceManifest(
			file.Bytes,
			packagePath == ".",
			source.PackageManager,
		)
		if err != nil {
			return invalidDependencySource(
				"local package %q manifest: %v",
				packagePath,
				err,
			)
		}
		if !equalOptionalString(file.Name, manifest.Name) ||
			!equalOptionalString(file.Version, manifest.Version) {
			return invalidDependencySource(
				"local package %q metadata is inconsistent",
				packagePath,
			)
		}
	}
	for i := range pins {
		if pins[i] != source.RegistryPins[i] {
			return invalidDependencySource("registry pin set is inconsistent")
		}
	}
	return validateDependencyProject(source)
}

func copyOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func DependencyFailureReason(err error) (BuildFailureReason, bool) {
	switch {
	case errors.Is(err, ErrLockfileUnsupported):
		return BuildFailureLockfileUnsupported, true
	case errors.Is(err, ErrDependencySourceInvalid):
		return BuildFailureInvalidSource, true
	case errors.Is(err, ErrDependencyOutputInvalid):
		return BuildFailureOutputInvalid, true
	default:
		return "", false
	}
}

func parseDependencyLockfile(
	manager PackageManagerName,
	raw []byte,
) (lockAnalysis, error) {
	switch manager {
	case PackageManagerBun:
		return parseBunLock(raw)
	case PackageManagerNPM:
		return parseNPMLock(raw)
	default:
		return lockAnalysis{}, unsupportedLockfile(
			"package manager %q has no lockfile adapter",
			manager,
		)
	}
}

func canonicalLocalPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, unsupportedLockfile("lockfile has no local packages")
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, packagePath := range paths {
		if packagePath == "" {
			packagePath = "."
		}
		if packagePath != "." {
			if err := validatePackagePath(packagePath, programMountPath, true); err != nil {
				return nil, invalidDependencySource(
					"local package path %q: %v",
					packagePath,
					err,
				)
			}
		}
		if _, exists := seen[packagePath]; exists {
			return nil, unsupportedLockfile(
				"duplicate local package path %q",
				packagePath,
			)
		}
		seen[packagePath] = struct{}{}
		out = append(out, packagePath)
	}
	if _, exists := seen["."]; !exists {
		return nil, unsupportedLockfile("lockfile has no root package")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i] == "." {
			return true
		}
		if out[j] == "." {
			return false
		}
		return out[i] < out[j]
	})
	return out, nil
}

func canonicalRegistryPins(pins []RegistryPin) ([]RegistryPin, error) {
	byKey := make(map[string]RegistryPin, len(pins))
	byIdentity := make(map[string]string, len(pins))
	for _, pin := range pins {
		if err := validatePackageName(pin.Name); err != nil {
			return nil, unsupportedLockfile("registry package name: %v", err)
		}
		if len(pin.Version) > maxPackageVersionBytes ||
			!packageManagerVersionPattern.MatchString(pin.Version) {
			return nil, invalidDependencySource(
				"registry package %q version %q is not an exact SemVer release or prerelease",
				pin.Name,
				pin.Version,
			)
		}
		if err := validatePackageIntegrity(pin.Integrity); err != nil {
			return nil, invalidDependencySource(
				"registry package %q: %v",
				pin.Name,
				err,
			)
		}
		identity := pin.Name + "\x00" + pin.Version
		if integrity, exists := byIdentity[identity]; exists &&
			integrity != pin.Integrity {
			return nil, invalidDependencySource(
				"registry package %q version %q has divergent integrities",
				pin.Name,
				pin.Version,
			)
		}
		byIdentity[identity] = pin.Integrity
		byKey[registryPinKey(pin)] = pin
	}
	out := make([]RegistryPin, 0, len(byKey))
	for _, pin := range byKey {
		out = append(out, pin)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Integrity < out[j].Integrity
	})
	return out, nil
}

func registryPinKey(pin RegistryPin) string {
	return pin.Name + "\x00" + pin.Version + "\x00" + pin.Integrity
}

func validateDependencySourceLayout(root *os.Root, selectedLock string) error {
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return invalidDependencySource("inspect source path %q: %v", name, err)
		}
		switch path.Base(name) {
		case ".npmrc", "bunfig.toml":
			return invalidDependencySource(
				"package manager configuration %q is unsupported",
				name,
			)
		}
		switch strings.ToLower(path.Base(name)) {
		case "bun.lock", "bun.lockb", "package-lock.json", "npm-shrinkwrap.json":
			if path.Dir(name) == "." && name != selectedLock {
				return invalidDependencySource(
					"conflicting root lockfile %q is present",
					name,
				)
			}
		}
		return nil
	})
}

func readDependencyFile(root *os.Root, name string, limit int64) ([]byte, error) {
	if !utf8.ValidString(name) ||
		strings.ContainsRune(name, 0) ||
		strings.ContainsRune(name, '\\') ||
		path.IsAbs(name) ||
		path.Clean(name) != name {
		return nil, fmt.Errorf("path %q is not normalized relative UTF-8", name)
	}
	components := strings.Split(name, "/")
	for i := range components {
		current := strings.Join(components[:i+1], "/")
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %q traverses symbolic link %q", name, current)
		}
		if i < len(components)-1 && !info.IsDir() {
			return nil, fmt.Errorf("path %q traverses non-directory %q", name, current)
		}
		if i == len(components)-1 {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("path %q is not a regular file", name)
			}
			if info.Size() < 1 || info.Size() > limit {
				return nil, fmt.Errorf(
					"path %q size is outside [1,%d]",
					name,
					limit,
				)
			}
		}
	}
	raw, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) < 1 || int64(len(raw)) > limit {
		return nil, fmt.Errorf("path %q size is outside [1,%d]", name, limit)
	}
	return raw, nil
}

func validateSourceManifest(
	raw []byte,
	root bool,
	manager PackageManager,
) (packageManifest, error) {
	manifest, err := parseLocalPackageManifest(raw, root)
	if err != nil {
		return packageManifest{}, err
	}
	if len(manifest.AutomaticScripts) != 0 {
		return packageManifest{}, fmt.Errorf(
			"automatic lifecycle script %q is unsupported",
			manifest.AutomaticScripts[0],
		)
	}
	object, err := decodePackageManifest(raw)
	if err != nil {
		return packageManifest{}, err
	}
	if root {
		want := string(manager.Name) + "@" + manager.Version
		if manifest.PackageManager == nil || *manifest.PackageManager != want {
			return packageManifest{}, fmt.Errorf("packageManager does not equal %q", want)
		}
	} else if _, exists := object["workspaces"]; exists {
		return packageManifest{}, errors.New("nested package workspaces are unsupported")
	}
	if _, exists := object["patchedDependencies"]; exists {
		return packageManifest{}, errors.New("patchedDependencies is unsupported")
	}
	for _, field := range []string{
		"dependencies",
		"devDependencies",
		"optionalDependencies",
		"peerDependencies",
	} {
		if err := rejectProhibitedManifestSources(object[field], field); err != nil {
			return packageManifest{}, err
		}
	}

	for _, field := range []string{
		"trustedDependencies",
		"nativeDependencies",
		"ignoreScripts",
	} {
		value, exists := object[field]
		if !exists {
			continue
		}
		if manager.Name != PackageManagerBun || !root {
			return packageManifest{}, fmt.Errorf("%s is supported only by a Bun root package", field)
		}
		values, ok := value.([]any)
		if !ok || len(values) > maxBunDependencyNames {
			return packageManifest{}, fmt.Errorf(
				"%s must be an array of at most %d package names",
				field,
				maxBunDependencyNames,
			)
		}
		seen := make(map[string]struct{}, len(values))
		for _, item := range values {
			name, ok := item.(string)
			if !ok {
				return packageManifest{}, fmt.Errorf("%s must contain only package names", field)
			}
			if err := validatePackageName(name); err != nil {
				return packageManifest{}, fmt.Errorf("%s: %v", field, err)
			}
			if _, exists := seen[name]; exists {
				return packageManifest{}, fmt.Errorf("%s contains duplicate package %q", field, name)
			}
			seen[name] = struct{}{}
		}
	}
	return manifest, nil
}

func rejectProhibitedManifestSources(value any, field string) error {
	dependencies, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for name, raw := range dependencies {
		spec, ok := raw.(string)
		if !ok {
			continue
		}
		if prohibitedDependencySpecifier(spec) {
			return fmt.Errorf(
				"%s package %q uses unsupported source %q",
				field,
				name,
				spec,
			)
		}
	}
	return nil
}

func prohibitedDependencySpecifier(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(lower, "\\") ||
		(strings.Contains(lower, "/") &&
			!strings.HasPrefix(lower, "npm:") &&
			!strings.HasPrefix(lower, "workspace:")) ||
		strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar.gz") {
		return true
	}
	for _, prefix := range []string{
		"file:",
		"link:",
		"git:",
		"git+",
		"git@",
		"github:",
		"gitlab:",
		"bitbucket:",
		"ssh:",
		"http:",
		"https:",
		"tarball:",
		"./",
		"../",
		"/",
		"~/",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func decodeLockJSON(raw []byte, jsonc bool) (map[string]any, error) {
	if len(raw) == 0 || int64(len(raw)) > maxLockfileBytes {
		return nil, unsupportedLockfile(
			"lockfile size is outside [1,%d]",
			maxLockfileBytes,
		)
	}
	if !utf8.Valid(raw) {
		return nil, unsupportedLockfile("lockfile is not valid UTF-8")
	}
	body := raw
	var err error
	if jsonc {
		body, err = normalizeJSONC(raw)
		if err != nil {
			return nil, unsupportedLockfile("%v", err)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder, "lockfile")
	if err != nil {
		return nil, unsupportedLockfile("%v", err)
	}
	if err := ensureEOF(decoder, "lockfile"); err != nil {
		return nil, unsupportedLockfile("%v", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, unsupportedLockfile("lockfile root must be an object")
	}
	return object, nil
}

func normalizeJSONC(raw []byte) ([]byte, error) {
	withoutComments := append([]byte(nil), raw...)
	inString := false
	escaped := false
	for i := 0; i < len(withoutComments); i++ {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if withoutComments[i] == '\\' {
				escaped = true
			} else if withoutComments[i] == '"' {
				inString = false
			}
		case withoutComments[i] == '"':
			inString = true
		case withoutComments[i] == '/' &&
			i+1 < len(withoutComments) &&
			withoutComments[i+1] == '/':
			withoutComments[i], withoutComments[i+1] = ' ', ' '
			i += 2
			for ; i < len(withoutComments) && withoutComments[i] != '\n'; i++ {
				withoutComments[i] = ' '
			}
			i--
		case withoutComments[i] == '/' &&
			i+1 < len(withoutComments) &&
			withoutComments[i+1] == '*':
			withoutComments[i], withoutComments[i+1] = ' ', ' '
			i += 2
			closed := false
			for ; i < len(withoutComments); i++ {
				if withoutComments[i] == '*' &&
					i+1 < len(withoutComments) &&
					withoutComments[i+1] == '/' {
					withoutComments[i], withoutComments[i+1] = ' ', ' '
					i++
					closed = true
					break
				}
				if withoutComments[i] != '\n' && withoutComments[i] != '\r' {
					withoutComments[i] = ' '
				}
			}
			if !closed {
				return nil, errors.New("lockfile contains an unterminated block comment")
			}
		}
	}

	out := append([]byte(nil), withoutComments...)
	inString, escaped = false, false
	for i := 0; i < len(out); i++ {
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if out[i] == '\\' {
				escaped = true
			} else if out[i] == '"' {
				inString = false
			}
		case out[i] == '"':
			inString = true
		case out[i] == ',':
			j := i + 1
			for j < len(out) {
				switch out[j] {
				case ' ', '\t', '\r', '\n':
					j++
				default:
					goto nextToken
				}
			}
		nextToken:
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}
	return out, nil
}

func requireOnlyFields(
	object map[string]any,
	label string,
	fields ...string,
) error {
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for field := range object {
		if _, ok := allowed[field]; !ok {
			return unsupportedLockfile(
				"%s contains unknown semantic member %q",
				label,
				field,
			)
		}
	}
	return nil
}

func exactJSONInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	integer, err := number.Int64()
	return integer, err == nil
}

func unsupportedLockfile(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLockfileUnsupported, fmt.Sprintf(format, args...))
}

func invalidDependencySource(format string, args ...any) error {
	return fmt.Errorf(
		"%w: %s",
		ErrDependencySourceInvalid,
		fmt.Sprintf(format, args...),
	)
}

func invalidDependencyOutput(format string, args ...any) error {
	return fmt.Errorf(
		"%w: %s",
		ErrDependencyOutputInvalid,
		fmt.Sprintf(format, args...),
	)
}
