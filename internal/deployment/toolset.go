package deployment

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ToolsetFormatVersion = 0

	ManagerComponentMediaType = "application/vnd.helmr.package-manager-component.v0+squashfs"
	ToolchainMediaType        = "application/vnd.helmr.standard-toolchain.v0+squashfs"

	maxManagerRegistrationBytes       = 32 << 10
	maxToolchainBytes                 = 4 << 10
	maxToolsetBytes                   = 64 << 10
	maxToolComponentsBytes            = 64 << 10
	maxToolRegistryBytes              = 16 << 20
	maxToolRegistryMembers            = 1024
	maxToolArgBytes                   = 4096
	maxToolArgvBytes                  = 16 << 10
	maxToolEnvironmentBytes           = 16 << 10
	maxToolArtifactBytes        int64 = 4 << 30

	managerRegistrationDigestDomain = "helmr.package-manager-registration.v0\x00"
	toolchainDigestDomain           = "helmr.standard-toolchain.v0\x00"
	toolsetDigestDomain             = "helmr.dependency-toolset.v0\x00"
	toolComponentsDigestDomain      = "helmr.dependency-toolset-components.v0\x00"
)

var (
	toolEnvironmentNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	lockfileAdapterPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

type ToolCommand struct {
	Argv []string `json:"argv"`
}

type ToolVersionProbe struct {
	Argv         []string `json:"argv"`
	StdoutBase64 string   `json:"stdoutBase64"`
}

type ToolOfflineStore struct {
	ReadOnlyMountPath string `json:"readOnlyMountPath"`
	WorkPath          string `json:"workPath"`
}

type ToolProxy struct {
	RegistryOrigin string `json:"registryOrigin"`
}

type ManagerRegistration struct {
	Architecture    RuntimeArchitecture `json:"architecture"`
	Executable      string              `json:"executable"`
	FixtureDigest   string              `json:"fixtureDigest"`
	FormatVersion   int                 `json:"formatVersion"`
	Lifecycle       ToolCommand         `json:"lifecycle"`
	LockfileAdapter string              `json:"lockfileAdapter"`
	ManagerClosure  ManagerArtifact     `json:"managerClosure"`
	OfflineStore    ToolOfflineStore    `json:"offlineStore"`
	PackageManager  PackageManager      `json:"packageManager"`
	Proxy           ToolProxy           `json:"proxy"`
	Resolution      ToolCommand         `json:"resolution"`
	VersionProbe    ToolVersionProbe    `json:"versionProbe"`
}

type Toolchain struct {
	Architecture         RuntimeArchitecture `json:"architecture"`
	FixtureDigest        string              `json:"fixtureDigest"`
	FormatVersion        int                 `json:"formatVersion"`
	ManagedRuntimeDigest string              `json:"managedRuntimeDigest"`
	ToolchainClosure     ManagerArtifact     `json:"toolchainClosure"`
}

type ToolEnvironment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Toolset struct {
	Architecture              RuntimeArchitecture `json:"architecture"`
	Artifact                  ManagerArtifact     `json:"artifact"`
	ComponentManifestDigest   string              `json:"componentManifestDigest"`
	Environment               []ToolEnvironment   `json:"environment"`
	FixtureDigest             string              `json:"fixtureDigest"`
	FormatVersion             int                 `json:"formatVersion"`
	ManagedRuntimeDigest      string              `json:"managedRuntimeDigest"`
	ManagerRegistrationDigest string              `json:"managerRegistrationDigest"`
	MaterializerVersion       string              `json:"materializerVersion"`
	PackageManager            PackageManager      `json:"packageManager"`
	StandardToolchainDigest   string              `json:"standardToolchainDigest"`
}

type ToolLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type ToolComponents struct {
	Architecture         RuntimeArchitecture `json:"architecture"`
	Environment          []ToolEnvironment   `json:"environment"`
	FormatVersion        int                 `json:"formatVersion"`
	Launchers            []ToolLink          `json:"launchers"`
	ManagedRuntimeDigest string              `json:"managedRuntimeDigest"`
	Manager              ManagerRegistration `json:"manager"`
	MaterializerVersion  string              `json:"materializerVersion"`
	PackageManager       PackageManager      `json:"packageManager"`
	SystemAliases        []ToolLink          `json:"systemAliases"`
	Toolchain            Toolchain           `json:"toolchain"`
}

type toolRegistryDocument struct {
	FormatVersion int                   `json:"formatVersion"`
	Managers      []ManagerRegistration `json:"managers"`
	Toolchains    []Toolchain           `json:"toolchains"`
	Toolsets      []Toolset             `json:"toolsets"`
}

type ToolRegistry struct {
	managers   []ManagerRegistration
	toolchains []Toolchain
	toolsets   []Toolset
}

func CanonicalManagerRegistration(value ManagerRegistration) ([]byte, error) {
	if err := validateManagerRegistration(value); err != nil {
		return nil, err
	}
	return canonicalToolDocument(value, "manager registration", maxManagerRegistrationBytes)
}

func ManagerRegistrationDigest(value ManagerRegistration) (string, error) {
	canonical, err := CanonicalManagerRegistration(value)
	if err != nil {
		return "", err
	}
	return toolDigest(managerRegistrationDigestDomain, canonical), nil
}

func CanonicalToolchain(value Toolchain) ([]byte, error) {
	if err := validateToolchain(value); err != nil {
		return nil, err
	}
	return canonicalToolDocument(value, "standard toolchain", maxToolchainBytes)
}

func StandardToolchainDigest(value Toolchain) (string, error) {
	canonical, err := CanonicalToolchain(value)
	if err != nil {
		return "", err
	}
	return toolDigest(toolchainDigestDomain, canonical), nil
}

func CanonicalToolset(value Toolset) ([]byte, error) {
	if err := validateToolset(value); err != nil {
		return nil, err
	}
	return canonicalToolDocument(value, "dependency toolset", maxToolsetBytes)
}

func DependencyToolsDigest(value Toolset) (string, error) {
	canonical, err := CanonicalToolset(value)
	if err != nil {
		return "", err
	}
	return toolDigest(toolsetDigestDomain, canonical), nil
}

func ParseToolComponents(raw []byte) (ToolComponents, error) {
	var value ToolComponents
	if err := parseToolDocument(raw, &value, "tool components", maxToolComponentsBytes); err != nil {
		return ToolComponents{}, err
	}
	if err := validateToolComponents(value); err != nil {
		return ToolComponents{}, err
	}
	complete, err := CanonicalToolComponents(value)
	if err != nil {
		return ToolComponents{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ToolComponents{}, errors.New("tool components do not match the complete canonical v0 shape")
	}
	return value, nil
}

func CanonicalToolComponents(value ToolComponents) ([]byte, error) {
	if err := validateToolComponents(value); err != nil {
		return nil, err
	}
	return canonicalToolDocument(value, "tool components", maxToolComponentsBytes)
}

func ComponentManifestDigest(value ToolComponents) (string, error) {
	canonical, err := CanonicalToolComponents(value)
	if err != nil {
		return "", err
	}
	return toolDigest(toolComponentsDigestDomain, canonical), nil
}

func ParseToolRegistry(raw []byte) (*ToolRegistry, error) {
	var document toolRegistryDocument
	if err := parseToolDocument(raw, &document, "tool registry", maxToolRegistryBytes); err != nil {
		return nil, err
	}
	if err := validateToolRegistry(document); err != nil {
		return nil, err
	}
	complete, err := canonicalToolDocument(document, "tool registry", maxToolRegistryBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, errors.New("tool registry does not match the complete canonical v0 shape")
	}
	return &ToolRegistry{
		managers:   append([]ManagerRegistration(nil), document.Managers...),
		toolchains: append([]Toolchain(nil), document.Toolchains...),
		toolsets:   append([]Toolset(nil), document.Toolsets...),
	}, nil
}

func CanonicalToolRegistry(
	managers []ManagerRegistration,
	toolchains []Toolchain,
	toolsets []Toolset,
) ([]byte, error) {
	document := toolRegistryDocument{
		FormatVersion: ToolsetFormatVersion,
		Managers:      append([]ManagerRegistration(nil), managers...),
		Toolchains:    append([]Toolchain(nil), toolchains...),
		Toolsets:      append([]Toolset(nil), toolsets...),
	}
	if err := validateToolRegistry(document); err != nil {
		return nil, err
	}
	return canonicalToolDocument(document, "tool registry", maxToolRegistryBytes)
}

func ValidateToolComponents(toolset Toolset, components ToolComponents) error {
	if err := validateToolset(toolset); err != nil {
		return err
	}
	if err := validateToolComponents(components); err != nil {
		return err
	}
	managerDigest, err := ManagerRegistrationDigest(components.Manager)
	if err != nil {
		return err
	}
	toolchainDigest, err := StandardToolchainDigest(components.Toolchain)
	if err != nil {
		return err
	}
	componentDigest, err := ComponentManifestDigest(components)
	if err != nil {
		return err
	}
	if componentDigest != toolset.ComponentManifestDigest ||
		managerDigest != toolset.ManagerRegistrationDigest ||
		toolchainDigest != toolset.StandardToolchainDigest ||
		components.Architecture != toolset.Architecture ||
		components.ManagedRuntimeDigest != toolset.ManagedRuntimeDigest ||
		components.MaterializerVersion != toolset.MaterializerVersion ||
		components.PackageManager != toolset.PackageManager ||
		!equalToolEnvironment(components.Environment, toolset.Environment) {
		return errors.New("tool components do not match the dependency toolset")
	}
	return nil
}

func validateManagerRegistration(value ManagerRegistration) error {
	if value.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf("manager registration formatVersion = %d, want %d", value.FormatVersion, ToolsetFormatVersion)
	}
	if !validArchitecture(value.Architecture) {
		return fmt.Errorf("manager registration architecture %q is unsupported", value.Architecture)
	}
	if err := validateManagerPackage(value.PackageManager); err != nil {
		return err
	}
	if !validToolDigest(value.FixtureDigest) {
		return errors.New("manager registration fixtureDigest is invalid")
	}
	if err := validateToolArtifact(value.ManagerClosure, ManagerComponentMediaType, "manager closure"); err != nil {
		return err
	}
	if !validManagedToolPath(value.Executable) {
		return errors.New("manager registration executable is outside the managed tool bin")
	}
	if err := validateToolCommand(value.Resolution, value.Executable, "resolution"); err != nil {
		return err
	}
	if err := validateToolCommand(value.Lifecycle, value.Executable, "lifecycle"); err != nil {
		return err
	}
	if err := validateToolCommand(ToolCommand{Argv: value.VersionProbe.Argv}, value.Executable, "version probe"); err != nil {
		return err
	}
	probe, err := base64.StdEncoding.Strict().DecodeString(value.VersionProbe.StdoutBase64)
	if err != nil || len(probe) == 0 || len(probe) > 4096 || base64.StdEncoding.EncodeToString(probe) != value.VersionProbe.StdoutBase64 {
		return errors.New("manager registration version probe stdoutBase64 is invalid")
	}
	if !lockfileAdapterPattern.MatchString(value.LockfileAdapter) {
		return errors.New("manager registration lockfileAdapter is invalid")
	}
	if !validAbsoluteToolPath(value.OfflineStore.ReadOnlyMountPath, "/opt/helmr/") ||
		!validAbsoluteToolPath(value.OfflineStore.WorkPath, "/work/") {
		return errors.New("manager registration offline store paths are invalid")
	}
	if err := validateRegistryOrigin(value.Proxy.RegistryOrigin); err != nil {
		return err
	}
	return nil
}

func validateToolchain(value Toolchain) error {
	if value.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf("standard toolchain formatVersion = %d, want %d", value.FormatVersion, ToolsetFormatVersion)
	}
	if !validArchitecture(value.Architecture) {
		return fmt.Errorf("standard toolchain architecture %q is unsupported", value.Architecture)
	}
	if !validToolDigest(value.FixtureDigest) || !validToolDigest(value.ManagedRuntimeDigest) {
		return errors.New("standard toolchain digest is invalid")
	}
	return validateToolArtifact(value.ToolchainClosure, ToolchainMediaType, "toolchain closure")
}

func validateToolset(value Toolset) error {
	if value.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf("dependency toolset formatVersion = %d, want %d", value.FormatVersion, ToolsetFormatVersion)
	}
	if !validArchitecture(value.Architecture) {
		return fmt.Errorf("dependency toolset architecture %q is unsupported", value.Architecture)
	}
	if err := validateManagerPackage(value.PackageManager); err != nil {
		return err
	}
	if value.MaterializerVersion != DependencyMaterializerVersion {
		return fmt.Errorf("dependency toolset materializerVersion = %q, want %q", value.MaterializerVersion, DependencyMaterializerVersion)
	}
	for _, digest := range []string{
		value.ComponentManifestDigest,
		value.FixtureDigest,
		value.ManagedRuntimeDigest,
		value.ManagerRegistrationDigest,
		value.StandardToolchainDigest,
	} {
		if !validToolDigest(digest) {
			return errors.New("dependency toolset digest is invalid")
		}
	}
	if err := validateToolArtifact(value.Artifact, ManagerDependencyToolsMediaType, "dependency tools"); err != nil {
		return err
	}
	return validateToolEnvironment(value.Environment)
}

func validateToolComponents(value ToolComponents) error {
	if value.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf("tool components formatVersion = %d, want %d", value.FormatVersion, ToolsetFormatVersion)
	}
	if !validArchitecture(value.Architecture) ||
		value.Manager.Architecture != value.Architecture ||
		value.Toolchain.Architecture != value.Architecture {
		return errors.New("tool components architecture is inconsistent")
	}
	if value.MaterializerVersion != DependencyMaterializerVersion {
		return errors.New("tool components materializerVersion is invalid")
	}
	if !validToolDigest(value.ManagedRuntimeDigest) ||
		value.Toolchain.ManagedRuntimeDigest != value.ManagedRuntimeDigest {
		return errors.New("tool components managedRuntimeDigest is inconsistent")
	}
	if err := validateManagerRegistration(value.Manager); err != nil {
		return err
	}
	if _, err := CanonicalManagerRegistration(value.Manager); err != nil {
		return err
	}
	if err := validateToolchain(value.Toolchain); err != nil {
		return err
	}
	if _, err := CanonicalToolchain(value.Toolchain); err != nil {
		return err
	}
	if value.PackageManager != value.Manager.PackageManager {
		return errors.New("tool components packageManager is inconsistent")
	}
	if err := validateToolEnvironment(value.Environment); err != nil {
		return err
	}
	if err := validateToolLinks(value.Launchers, false); err != nil {
		return err
	}
	managerLauncher := "bin/" + strings.TrimPrefix(
		value.Manager.Executable,
		"/opt/helmr/dependency-tools/bin/",
	)
	if !hasToolLink(value.Launchers, managerLauncher) {
		return errors.New("tool components omit the manager launcher")
	}
	if err := validateToolLinks(value.SystemAliases, true); err != nil {
		return err
	}
	return nil
}

func validateToolRegistry(document toolRegistryDocument) error {
	if document.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf("tool registry formatVersion = %d, want %d", document.FormatVersion, ToolsetFormatVersion)
	}
	if len(document.Managers) == 0 || len(document.Managers) > maxToolRegistryMembers ||
		len(document.Toolchains) == 0 || len(document.Toolchains) > maxToolRegistryMembers ||
		len(document.Toolsets) == 0 || len(document.Toolsets) > maxToolRegistryMembers {
		return errors.New("tool registry arrays are outside their bounds")
	}
	managerDigests := make(map[string]ManagerRegistration, len(document.Managers))
	for index, manager := range document.Managers {
		if err := validateManagerRegistration(manager); err != nil {
			return fmt.Errorf("tool registry manager %d: %w", index, err)
		}
		if index > 0 && compareManagers(document.Managers[index-1], manager) >= 0 {
			return errors.New("tool registry managers are not in key order")
		}
		digest, err := ManagerRegistrationDigest(manager)
		if err != nil {
			return fmt.Errorf("tool registry manager %d: %w", index, err)
		}
		if _, exists := managerDigests[digest]; exists {
			return errors.New("tool registry has a duplicate manager digest")
		}
		managerDigests[digest] = manager
	}
	toolchainDigests := make(map[string]Toolchain, len(document.Toolchains))
	previousToolchainDigest := ""
	for index, toolchain := range document.Toolchains {
		if err := validateToolchain(toolchain); err != nil {
			return fmt.Errorf("tool registry toolchain %d: %w", index, err)
		}
		digest, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return fmt.Errorf("tool registry toolchain %d: %w", index, err)
		}
		if index > 0 && compareToolchains(
			document.Toolchains[index-1],
			toolchain,
			previousToolchainDigest,
			digest,
		) >= 0 {
			return errors.New("tool registry toolchains are not in key order")
		}
		if _, exists := toolchainDigests[digest]; exists {
			return errors.New("tool registry has a duplicate toolchain digest")
		}
		toolchainDigests[digest] = toolchain
		previousToolchainDigest = digest
	}
	toolsetDigests := make(map[string]struct{}, len(document.Toolsets))
	for index, toolset := range document.Toolsets {
		if err := validateToolset(toolset); err != nil {
			return fmt.Errorf("tool registry toolset %d: %w", index, err)
		}
		if index > 0 && compareToolsets(document.Toolsets[index-1], toolset) >= 0 {
			return errors.New("tool registry toolsets are not in key order")
		}
		manager, managerExists := managerDigests[toolset.ManagerRegistrationDigest]
		toolchain, toolchainExists := toolchainDigests[toolset.StandardToolchainDigest]
		if !managerExists || !toolchainExists ||
			manager.PackageManager != toolset.PackageManager ||
			manager.Architecture != toolset.Architecture ||
			toolchain.Architecture != toolset.Architecture ||
			toolchain.ManagedRuntimeDigest != toolset.ManagedRuntimeDigest {
			return errors.New("tool registry toolset references inconsistent components")
		}
		digest, err := DependencyToolsDigest(toolset)
		if err != nil {
			return fmt.Errorf("tool registry toolset %d: %w", index, err)
		}
		if _, exists := toolsetDigests[digest]; exists {
			return errors.New("tool registry has a duplicate toolset digest")
		}
		toolsetDigests[digest] = struct{}{}
	}
	return nil
}

func validateToolArtifact(value ManagerArtifact, mediaType, label string) error {
	if !validToolDigest(value.Digest) {
		return fmt.Errorf("%s digest is invalid", label)
	}
	if value.MediaType != mediaType {
		return fmt.Errorf("%s mediaType = %q, want %q", label, value.MediaType, mediaType)
	}
	if value.SizeBytes < 1 || value.SizeBytes > maxToolArtifactBytes {
		return fmt.Errorf("%s sizeBytes is outside [1,%d]", label, maxToolArtifactBytes)
	}
	return nil
}

func validateToolCommand(value ToolCommand, executable, label string) error {
	if len(value.Argv) == 0 || len(value.Argv) > 64 || value.Argv[0] != executable {
		return fmt.Errorf("manager registration %s argv is invalid", label)
	}
	total := 0
	for _, argument := range value.Argv {
		size := len(argument)
		if size == 0 ||
			size > maxToolArgBytes ||
			!utf8.ValidString(argument) ||
			strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("manager registration %s argv is invalid", label)
		}
		total += size
	}
	if total > maxToolArgvBytes {
		return fmt.Errorf("manager registration %s argv is oversized", label)
	}
	return nil
}

func validateToolEnvironment(values []ToolEnvironment) error {
	if len(values) == 0 || len(values) > 64 {
		return errors.New("tool environment is outside its member bounds")
	}
	total := 0
	pathCount := 0
	for index, value := range values {
		if !toolEnvironmentNamePattern.MatchString(value.Name) ||
			len(value.Value) > maxToolArgBytes ||
			!utf8.ValidString(value.Value) ||
			strings.IndexByte(value.Value, 0) >= 0 {
			return errors.New("tool environment member is invalid")
		}
		if index > 0 && values[index-1].Name >= value.Name {
			return errors.New("tool environment is not in name order")
		}
		total += len(value.Name) + len(value.Value)
		if value.Name == "PATH" {
			pathCount++
			if err := validateToolPath(value.Value); err != nil {
				return err
			}
		}
	}
	if total > maxToolEnvironmentBytes || pathCount != 1 {
		return errors.New("tool environment must contain one bounded PATH")
	}
	return nil
}

func validateToolPath(value string) error {
	entries := strings.Split(value, ":")
	if len(entries) < 2 || entries[0] != "/opt/helmr/dependency-tools/bin" {
		return errors.New("tool environment PATH has an invalid launcher root")
	}
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if _, exists := seen[entry]; exists {
			return errors.New("tool environment PATH contains a duplicate")
		}
		seen[entry] = struct{}{}
		if index > 0 && !validAbsoluteToolPath(entry, "/nix/store/") {
			return errors.New("tool environment PATH contains an unregistered path")
		}
	}
	return nil
}

func validateToolLinks(values []ToolLink, system bool) error {
	if len(values) == 0 {
		return errors.New("tool links must be non-empty")
	}
	seenTargets := make(map[string]struct{}, len(values))
	requiredAliases := map[string]bool{"/bin/sh": false, "/usr/bin/env": false}
	for index, value := range values {
		if !utf8.ValidString(value.Path) || strings.IndexByte(value.Path, 0) >= 0 {
			return errors.New("tool link path is invalid")
		}
		if index > 0 && values[index-1].Path >= value.Path {
			return errors.New("tool links are not in path order")
		}
		if !validAbsoluteToolPath(value.Target, "/nix/store/") {
			return errors.New("tool link target is outside the store")
		}
		if system {
			if _, allowed := requiredAliases[value.Path]; !allowed {
				return errors.New("system alias path is not admitted")
			}
			requiredAliases[value.Path] = true
		} else {
			if _, exists := seenTargets[value.Target]; exists {
				return errors.New("launchers contain a duplicate target")
			}
			seenTargets[value.Target] = struct{}{}
			if path.Clean(value.Path) != value.Path ||
				!strings.HasPrefix(value.Path, "bin/") ||
				value.Path == "bin/" {
				return errors.New("launcher path is invalid")
			}
		}
	}
	if system && (!requiredAliases["/bin/sh"] || !requiredAliases["/usr/bin/env"] || len(values) != len(requiredAliases)) {
		return errors.New("system aliases are incomplete")
	}
	return nil
}

func validateRegistryOrigin(raw string) error {
	origin, err := url.Parse(raw)
	if err != nil ||
		origin.Scheme != "http" ||
		origin.Hostname() != "127.0.0.1" ||
		origin.Port() == "" ||
		origin.Path != "" ||
		origin.RawQuery != "" ||
		origin.Fragment != "" ||
		origin.User != nil {
		return errors.New("manager registration registryOrigin is invalid")
	}
	port, err := strconv.Atoi(origin.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != origin.Port() {
		return errors.New("manager registration registryOrigin port is invalid")
	}
	return nil
}

func validManagedToolPath(value string) bool {
	return validAbsoluteToolPath(value, "/opt/helmr/dependency-tools/bin/")
}

func validAbsoluteToolPath(value, prefix string) bool {
	return utf8.ValidString(value) &&
		strings.IndexByte(value, 0) < 0 &&
		path.IsAbs(value) &&
		path.Clean(value) == value &&
		strings.HasPrefix(value, prefix) &&
		value != strings.TrimSuffix(prefix, "/")
}

func validToolDigest(value string) bool {
	return sha256DigestPattern.MatchString(value)
}

func canonicalToolDocument(value any, label string, maxBytes int) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", label, err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(canonical) == 0 || len(canonical) > maxBytes {
		return nil, fmt.Errorf("%s size is outside [1,%d]", label, maxBytes)
	}
	return canonical, nil
}

func parseToolDocument(raw []byte, destination any, label string, maxBytes int) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return fmt.Errorf("%s size is outside [1,%d]", label, maxBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not RFC 8785 canonical JSON", label)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	return ensureEOF(decoder, label)
}

func toolDigest(domain string, canonical []byte) string {
	digest := domainDigest(domain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func compareManagers(left, right ManagerRegistration) int {
	return strings.Compare(
		fmt.Sprintf("%s\x00%s\x00%s", left.PackageManager.Name, left.PackageManager.Version, left.Architecture),
		fmt.Sprintf("%s\x00%s\x00%s", right.PackageManager.Name, right.PackageManager.Version, right.Architecture),
	)
}

func compareToolchains(left, right Toolchain, leftDigest, rightDigest string) int {
	return strings.Compare(
		left.ManagedRuntimeDigest+"\x00"+string(left.Architecture)+"\x00"+leftDigest,
		right.ManagedRuntimeDigest+"\x00"+string(right.Architecture)+"\x00"+rightDigest,
	)
}

func compareToolsets(left, right Toolset) int {
	leftKey := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		left.PackageManager.Name,
		left.PackageManager.Version,
		left.Architecture,
		left.ManagedRuntimeDigest,
		left.StandardToolchainDigest,
		left.MaterializerVersion,
	)
	rightKey := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		right.PackageManager.Name,
		right.PackageManager.Version,
		right.Architecture,
		right.ManagedRuntimeDigest,
		right.StandardToolchainDigest,
		right.MaterializerVersion,
	)
	return strings.Compare(leftKey, rightKey)
}

func equalToolEnvironment(left, right []ToolEnvironment) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hasToolLink(links []ToolLink, linkPath string) bool {
	for _, link := range links {
		if link.Path == linkPath {
			return true
		}
	}
	return false
}
