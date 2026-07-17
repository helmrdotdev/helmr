package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	PackageGraphFormatVersion = 0

	PackageKindLocal    = PackageKind("local")
	PackageKindRegistry = PackageKind("registry")

	PackageRelationshipProduction = PackageRelationship("production")
	PackageRelationshipOptional   = PackageRelationship("optional")
	PackageRelationshipPeer       = PackageRelationship("peer")

	maxPackageNameBytes       = 214
	maxPackageVersionBytes    = 255
	maxPackagePathComponent   = 255
	maxMountedPackagePath     = 4096
	sha512DigestBytes         = 64
	localPackageViewKeyDomain = "helmr.local-package-view.v0"
	programMountPath          = "/opt/helmr/program"
	dependencyMountPath       = "/opt/helmr/program/node_modules"
)

var packageNamePattern = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9._~-]*|@[a-z0-9][a-z0-9._~-]*/[a-z0-9][a-z0-9._~-]*)$`)

type PackageKind string

type PackageRelationship string

type LocalPackage struct {
	ManifestDigest string  `json:"manifestDigest"`
	Name           *string `json:"name"`
	Path           string  `json:"path"`
	Version        *string `json:"version"`
	ViewKey        *string `json:"viewKey"`
}

type RegistryPackage struct {
	InstallPath string `json:"installPath"`
	Integrity   string `json:"integrity"`
	Name        string `json:"name"`
	Version     string `json:"version"`
}

type PackageEndpoint struct {
	InstallPath *string     `json:"installPath,omitempty"`
	Kind        PackageKind `json:"kind"`
	Path        *string     `json:"path,omitempty"`
}

type PackageResolution struct {
	Dependency   string              `json:"dependency"`
	From         PackageEndpoint     `json:"from"`
	Relationship PackageRelationship `json:"relationship"`
	To           PackageEndpoint     `json:"to"`
}

type PackageGraph struct {
	FormatVersion    int                 `json:"formatVersion"`
	LocalPackages    []LocalPackage      `json:"localPackages"`
	RegistryPackages []RegistryPackage   `json:"registryPackages"`
	Resolutions      []PackageResolution `json:"resolutions"`
}

func ParsePackageGraph(raw []byte) (PackageGraph, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return PackageGraph{}, fmt.Errorf("package graph size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return PackageGraph{}, fmt.Errorf("canonicalize package graph: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return PackageGraph{}, fmt.Errorf("package graph is not RFC 8785 canonical JSON")
	}

	var graph PackageGraph
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&graph); err != nil {
		return PackageGraph{}, fmt.Errorf("decode package graph: %w", err)
	}
	if err := ensureEOF(decoder, "package graph"); err != nil {
		return PackageGraph{}, err
	}
	if err := ValidatePackageGraph(graph); err != nil {
		return PackageGraph{}, err
	}
	complete, err := CanonicalPackageGraph(graph)
	if err != nil {
		return PackageGraph{}, err
	}
	if !bytes.Equal(raw, complete) {
		return PackageGraph{}, fmt.Errorf("package graph does not match the complete canonical v0 shape")
	}
	return graph, nil
}

func CanonicalPackageGraph(graph PackageGraph) ([]byte, error) {
	if err := ValidatePackageGraph(graph); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(graph)
	if err != nil {
		return nil, fmt.Errorf("encode package graph: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize package graph: %w", err)
	}
	if len(canonical) > int(maxProgramFileSizeBytes) {
		return nil, fmt.Errorf("package graph size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	return canonical, nil
}

func ValidatePackageGraph(graph PackageGraph) error {
	if graph.FormatVersion != PackageGraphFormatVersion {
		return fmt.Errorf("package graph formatVersion = %d, want %d", graph.FormatVersion, PackageGraphFormatVersion)
	}
	if graph.LocalPackages == nil {
		return fmt.Errorf("package graph localPackages must be an array")
	}
	if graph.RegistryPackages == nil {
		return fmt.Errorf("package graph registryPackages must be an array")
	}
	if graph.Resolutions == nil {
		return fmt.Errorf("package graph resolutions must be an array")
	}
	if len(graph.LocalPackages) == 0 || graph.LocalPackages[0].Path != "." {
		return fmt.Errorf("package graph localPackages must begin with exactly one root record")
	}

	locals := make(map[string]LocalPackage, len(graph.LocalPackages))
	localNames := make(map[string]struct{}, len(graph.LocalPackages))
	viewKeys := make(map[string]struct{}, len(graph.LocalPackages))
	for position, local := range graph.LocalPackages {
		if err := validateLocalPackage(local, position == 0); err != nil {
			return fmt.Errorf("package graph localPackages[%d]: %w", position, err)
		}
		if _, exists := locals[local.Path]; exists {
			return fmt.Errorf("package graph localPackages contains duplicate path %q", local.Path)
		}
		locals[local.Path] = local
		if local.Name != nil {
			if _, exists := localNames[*local.Name]; exists {
				return fmt.Errorf("package graph localPackages contains ambiguous name %q", *local.Name)
			}
			localNames[*local.Name] = struct{}{}
		}
		if local.ViewKey != nil {
			if _, exists := viewKeys[*local.ViewKey]; exists {
				return fmt.Errorf("package graph localPackages contains colliding viewKey %q", *local.ViewKey)
			}
			viewKeys[*local.ViewKey] = struct{}{}
		}
		if position > 1 {
			previous := graph.LocalPackages[position-1].Path
			if bytes.Compare([]byte(previous), []byte(local.Path)) >= 0 {
				return fmt.Errorf("package graph localPackages are not in canonical path order at position %d", position)
			}
		}
		for separator := strings.IndexByte(local.Path, '/'); separator >= 0; {
			ancestor := local.Path[:separator]
			if _, exists := locals[ancestor]; exists {
				return fmt.Errorf("package graph non-root local paths %q and %q overlap", ancestor, local.Path)
			}
			next := strings.IndexByte(local.Path[separator+1:], '/')
			if next < 0 {
				break
			}
			separator += next + 1
		}
	}

	registries := make(map[string]struct{}, len(graph.RegistryPackages))
	for position, registry := range graph.RegistryPackages {
		if err := validateRegistryPackage(registry); err != nil {
			return fmt.Errorf("package graph registryPackages[%d]: %w", position, err)
		}
		if _, exists := registries[registry.InstallPath]; exists {
			return fmt.Errorf("package graph registryPackages contains duplicate installPath %q", registry.InstallPath)
		}
		registries[registry.InstallPath] = struct{}{}
		if position > 0 && bytes.Compare(
			[]byte(graph.RegistryPackages[position-1].InstallPath),
			[]byte(registry.InstallPath),
		) >= 0 {
			return fmt.Errorf("package graph registryPackages are not in canonical installPath order at position %d", position)
		}
	}

	for position, resolution := range graph.Resolutions {
		if err := validatePackageResolution(resolution, locals, registries); err != nil {
			return fmt.Errorf("package graph resolutions[%d]: %w", position, err)
		}
		if position > 0 && comparePackageResolutions(graph.Resolutions[position-1], resolution) >= 0 {
			return fmt.Errorf("package graph resolutions are not in canonical order at position %d", position)
		}
	}
	return nil
}

func validateLocalPackage(local LocalPackage, root bool) error {
	if !sha256DigestPattern.MatchString(local.ManifestDigest) {
		return fmt.Errorf("manifestDigest is not a lowercase SHA-256 digest")
	}
	if root {
		if local.Path != "." {
			return fmt.Errorf("root path = %q, want .", local.Path)
		}
		if local.ViewKey != nil {
			return fmt.Errorf("root viewKey must be null")
		}
	} else {
		if local.Path == "." {
			return fmt.Errorf("only the first local package may use root path .")
		}
		if err := validatePackagePath(local.Path, programMountPath, true); err != nil {
			return fmt.Errorf("path: %w", err)
		}
		if local.ViewKey == nil {
			return fmt.Errorf("non-root viewKey must be a string")
		}
		want := localPackageViewKey(local.Path)
		if *local.ViewKey != want {
			return fmt.Errorf("viewKey = %q, want %q", *local.ViewKey, want)
		}
	}
	if local.Name != nil {
		if err := validatePackageName(*local.Name); err != nil {
			return fmt.Errorf("name: %w", err)
		}
	}
	if local.Version != nil {
		if err := validatePackageVersion(*local.Version); err != nil {
			return fmt.Errorf("version: %w", err)
		}
	}
	return nil
}

func validateRegistryPackage(registry RegistryPackage) error {
	if err := validatePackagePath(registry.InstallPath, dependencyMountPath, false); err != nil {
		return fmt.Errorf("installPath: %w", err)
	}
	if err := validatePackageIntegrity(registry.Integrity); err != nil {
		return err
	}
	if err := validatePackageName(registry.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if err := validatePackageVersion(registry.Version); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	return nil
}

func validatePackageResolution(
	resolution PackageResolution,
	locals map[string]LocalPackage,
	registries map[string]struct{},
) error {
	if err := validatePackageName(resolution.Dependency); err != nil {
		return fmt.Errorf("dependency: %w", err)
	}
	switch resolution.Relationship {
	case PackageRelationshipProduction, PackageRelationshipOptional, PackageRelationshipPeer:
	default:
		return fmt.Errorf("relationship %q is unsupported", resolution.Relationship)
	}
	if err := validatePackageEndpoint(resolution.From, locals, registries); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := validatePackageEndpoint(resolution.To, locals, registries); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if resolution.To.Kind == PackageKindLocal {
		if resolution.From.Kind != PackageKindLocal {
			return fmt.Errorf("registry-to-local resolution is unsupported")
		}
		local := locals[*resolution.To.Path]
		if local.Name == nil {
			return fmt.Errorf("local target %q has no name", *resolution.To.Path)
		}
		if resolution.Dependency != *local.Name {
			return fmt.Errorf("dependency %q does not equal local target name %q", resolution.Dependency, *local.Name)
		}
	}
	return nil
}

func validatePackageEndpoint(
	endpoint PackageEndpoint,
	locals map[string]LocalPackage,
	registries map[string]struct{},
) error {
	switch endpoint.Kind {
	case PackageKindLocal:
		if endpoint.Path == nil || endpoint.InstallPath != nil {
			return fmt.Errorf("local endpoint must contain exactly kind and path")
		}
		if _, exists := locals[*endpoint.Path]; !exists {
			return fmt.Errorf("local path %q does not name a graph node", *endpoint.Path)
		}
	case PackageKindRegistry:
		if endpoint.InstallPath == nil || endpoint.Path != nil {
			return fmt.Errorf("registry endpoint must contain exactly installPath and kind")
		}
		if _, exists := registries[*endpoint.InstallPath]; !exists {
			return fmt.Errorf("registry installPath %q does not name a graph node", *endpoint.InstallPath)
		}
	default:
		return fmt.Errorf("kind %q is unsupported", endpoint.Kind)
	}
	return nil
}

func validatePackageName(name string) error {
	if len(name) == 0 || len(name) > maxPackageNameBytes || !packageNamePattern.MatchString(name) {
		return fmt.Errorf("%q is outside the exact package-name domain", name)
	}
	return nil
}

func validatePackageVersion(version string) error {
	if len(version) == 0 || len(version) > maxPackageVersionBytes || !utf8.ValidString(version) {
		return fmt.Errorf("%q is outside the exact package-version domain", version)
	}
	return nil
}

func validatePackageIntegrity(integrity string) error {
	const prefix = "sha512-"
	if !strings.HasPrefix(integrity, prefix) {
		return fmt.Errorf("integrity is not a canonical SHA-512 SRI value")
	}
	encoded := strings.TrimPrefix(integrity, prefix)
	digest, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(digest) != sha512DigestBytes || base64.StdEncoding.EncodeToString(digest) != encoded {
		return fmt.Errorf("integrity is not a canonical SHA-512 SRI value")
	}
	return nil
}

func validatePackagePath(path, mountPath string, local bool) error {
	if path == "" || !utf8.ValidString(path) || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return fmt.Errorf("%q is not a confined relative POSIX path", path)
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return fmt.Errorf("%q contains a control character", path)
		}
	}
	components := strings.Split(path, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("%q is not normalized", path)
		}
		if len(component) > maxPackagePathComponent {
			return fmt.Errorf("%q contains a component over %d bytes", path, maxPackagePathComponent)
		}
	}
	root := components[0]
	if local && (root == "helmr" || root == ".helmr" || root == "node_modules") {
		return fmt.Errorf("%q uses reserved Deployment root %q", path, root)
	}
	if !local && root == ".helmr" {
		return fmt.Errorf("%q uses reserved dependency root %q", path, root)
	}
	if len(mountPath)+1+len(path)+1 > maxMountedPackagePath {
		return fmt.Errorf("%q exceeds the mounted path bound", path)
	}
	return nil
}

func localPackageViewKey(path string) string {
	hash := sha256.New()
	hash.Write([]byte(localPackageViewKeyDomain))
	hash.Write([]byte{0})
	hash.Write([]byte(path))
	return hex.EncodeToString(hash.Sum(nil))
}

func comparePackageResolutions(left, right PackageResolution) int {
	if comparison := comparePackageEndpoints(left.From, right.From); comparison != 0 {
		return comparison
	}
	if comparison := bytes.Compare([]byte(left.Dependency), []byte(right.Dependency)); comparison != 0 {
		return comparison
	}
	if comparison := compareInts(
		packageRelationshipOrder(left.Relationship),
		packageRelationshipOrder(right.Relationship),
	); comparison != 0 {
		return comparison
	}
	return comparePackageEndpoints(left.To, right.To)
}

func comparePackageEndpoints(left, right PackageEndpoint) int {
	if comparison := compareInts(packageKindOrder(left.Kind), packageKindOrder(right.Kind)); comparison != 0 {
		return comparison
	}
	return bytes.Compare([]byte(packageEndpointLocator(left)), []byte(packageEndpointLocator(right)))
}

func packageEndpointLocator(endpoint PackageEndpoint) string {
	if endpoint.Kind == PackageKindLocal && endpoint.Path != nil {
		return *endpoint.Path
	}
	if endpoint.Kind == PackageKindRegistry && endpoint.InstallPath != nil {
		return *endpoint.InstallPath
	}
	return ""
}

func packageKindOrder(kind PackageKind) int {
	if kind == PackageKindLocal {
		return 0
	}
	if kind == PackageKindRegistry {
		return 1
	}
	return 2
}

func packageRelationshipOrder(relationship PackageRelationship) int {
	switch relationship {
	case PackageRelationshipProduction:
		return 0
	case PackageRelationshipOptional:
		return 1
	case PackageRelationshipPeer:
		return 2
	default:
		return 3
	}
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
