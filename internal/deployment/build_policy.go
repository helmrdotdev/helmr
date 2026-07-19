package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	regionpkg "github.com/helmrdotdev/helmr/internal/region"
)

const (
	BuildPolicyFormatVersion = 0
	maxBuildPolicyBytes      = maxProgramFileSizeBytes
)

var (
	ErrBuildRegionNotConfigured = errors.New("build policy region is not configured")
	ErrRuntimeNotRegistered     = errors.New("runtime is not registered")
)

type BuildTarget struct {
	Runtime                 RuntimeDescriptor
	StandardToolchainDigest string
	MaterializerVersion     string
}

type BuildPolicy struct {
	current            map[string]buildPolicyTarget
	runtimes           map[string]RuntimeDescriptor
	runtimesBytes      []byte
	toolRegistryDigest string
	registry           *ToolRegistry
}

type buildPolicyTarget struct {
	MaterializerVersion     string `json:"materializerVersion"`
	RuntimeDigest           string `json:"runtimeDigest"`
	StandardToolchainDigest string `json:"standardToolchainDigest"`
}

type buildPolicyDocument struct {
	Current            map[string]buildPolicyTarget `json:"current"`
	FormatVersion      int                          `json:"formatVersion"`
	Runtimes           []RuntimeDescriptor          `json:"runtimes"`
	ToolRegistryDigest string                       `json:"toolRegistryDigest"`
}

func LoadBuildPolicy(
	path string,
	catalog *RuntimeCatalog,
	registry *ToolRegistry,
) (*BuildPolicy, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open build policy: %w", err)
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxBuildPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read build policy: %w", err)
	}
	policy, err := ParseBuildPolicy(raw)
	if err != nil {
		return nil, err
	}
	if err := validateBuildPolicyRegistries(policy, catalog, registry); err != nil {
		return nil, err
	}
	return policy, nil
}

func ParseBuildPolicy(raw []byte) (*BuildPolicy, error) {
	if len(raw) == 0 || int64(len(raw)) > maxBuildPolicyBytes {
		return nil, fmt.Errorf(
			"build policy size is outside [1,%d]",
			maxBuildPolicyBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize build policy: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("build policy is not RFC 8785 canonical JSON")
	}

	var document buildPolicyDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode build policy: %w", err)
	}
	if err := ensureEOF(decoder, "build policy"); err != nil {
		return nil, err
	}
	if err := validateBuildPolicyDocument(document); err != nil {
		return nil, err
	}
	complete, err := canonicalBuildPolicyDocument(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, errors.New("build policy does not match the complete canonical v0 shape")
	}
	runtimesBytes, err := canonicalRuntimeDescriptors(document.Runtimes)
	if err != nil {
		return nil, err
	}
	policy := &BuildPolicy{
		current:            make(map[string]buildPolicyTarget, len(document.Current)),
		runtimes:           make(map[string]RuntimeDescriptor, len(document.Runtimes)),
		runtimesBytes:      runtimesBytes,
		toolRegistryDigest: document.ToolRegistryDigest,
	}
	for region, target := range document.Current {
		policy.current[region] = target
	}
	for _, descriptor := range document.Runtimes {
		policy.runtimes[descriptor.Digest] = descriptor
	}
	return policy, nil
}

func (p *BuildPolicy) Current(region string) (BuildTarget, error) {
	if p == nil {
		return BuildTarget{}, fmt.Errorf("%w: %q", ErrBuildRegionNotConfigured, region)
	}
	target, ok := p.current[region]
	if !ok {
		return BuildTarget{}, fmt.Errorf("%w: %q", ErrBuildRegionNotConfigured, region)
	}
	return BuildTarget{
		Runtime:                 p.runtimes[target.RuntimeDigest],
		StandardToolchainDigest: target.StandardToolchainDigest,
		MaterializerVersion:     target.MaterializerVersion,
	}, nil
}

func (p *BuildPolicy) ToolRegistryDigest() (string, error) {
	if p == nil || !validToolDigest(p.toolRegistryDigest) {
		return "", errToolRegistryUnauthenticated
	}
	return p.toolRegistryDigest, nil
}

func (p *BuildPolicy) ResolveRuntime(digest string) (RuntimeDescriptor, error) {
	if p == nil {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeNotRegistered, digest)
	}
	descriptor, ok := p.runtimes[digest]
	if !ok {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeNotRegistered, digest)
	}
	return descriptor, nil
}

func (p *BuildPolicy) Resolve(
	runtimeDigest,
	standardToolchainDigest,
	materializerVersion string,
) (BuildTarget, error) {
	runtime, err := p.ResolveRuntime(runtimeDigest)
	if err != nil {
		return BuildTarget{}, err
	}
	if p.registry == nil {
		return BuildTarget{}, errToolRegistryUnauthenticated
	}
	toolchain, err := p.registry.Toolchain(standardToolchainDigest)
	if err != nil {
		return BuildTarget{}, err
	}
	if toolchain.Architecture != runtime.Architecture ||
		toolchain.ManagedRuntimeDigest != runtime.Digest ||
		materializerVersion != DependencyMaterializerVersion {
		return BuildTarget{}, errors.New("build target is inconsistent")
	}
	return BuildTarget{
		Runtime:                 runtime,
		StandardToolchainDigest: standardToolchainDigest,
		MaterializerVersion:     materializerVersion,
	}, nil
}

func (p *BuildPolicy) ResolveToolset(
	target BuildTarget,
	manager PackageManager,
) (Toolset, error) {
	resolved, err := p.Resolve(
		target.Runtime.Digest,
		target.StandardToolchainDigest,
		target.MaterializerVersion,
	)
	if err != nil {
		return Toolset{}, err
	}
	if resolved != target {
		return Toolset{}, errors.New("build target does not exact-match the authenticated policy")
	}
	return p.registry.Resolve(ToolKey{
		Architecture:            resolved.Runtime.Architecture,
		ManagedRuntimeDigest:    resolved.Runtime.Digest,
		MaterializerVersion:     resolved.MaterializerVersion,
		PackageManager:          manager,
		StandardToolchainDigest: resolved.StandardToolchainDigest,
	})
}

func ValidateProgramTarget(
	target BuildTarget,
	receipt ProgramReceipt,
) error {
	if err := ValidateProgramReceipt(receipt); err != nil {
		return err
	}
	if receipt.Index.RuntimeDigest != target.Runtime.Digest ||
		receipt.Index.Architecture != target.Runtime.Architecture ||
		receipt.DependencyIndex.MaterializerVersion != target.MaterializerVersion ||
		receipt.DependencyPlan.ManagedRuntimeDigest != target.Runtime.Digest ||
		receipt.DependencyPlan.StandardToolchainDigest != target.StandardToolchainDigest ||
		receipt.DependencyPlan.MaterializerVersion != target.MaterializerVersion ||
		receipt.DependencyPlan.Architecture != target.Runtime.Architecture {
		return errors.New("program receipt does not match the build target")
	}
	return nil
}

func ValidateBuildPolicyUpgrade(previous, next *BuildPolicy) error {
	if previous == nil || next == nil {
		return errors.New("build policy upgrade requires both snapshots")
	}
	for digest, descriptor := range previous.runtimes {
		replacement, ok := next.runtimes[digest]
		if !ok {
			return fmt.Errorf("build policy upgrade removes registered runtime %q", digest)
		}
		if replacement != descriptor {
			return fmt.Errorf("build policy upgrade mutates registered runtime %q", digest)
		}
	}
	return nil
}

func validateBuildPolicyRegistries(
	policy *BuildPolicy,
	catalog *RuntimeCatalog,
	registry *ToolRegistry,
) error {
	if policy == nil {
		return errors.New("build policy is required")
	}
	if catalog == nil || !catalog.authenticated {
		return errors.New("authenticated runtime catalog is required")
	}
	if !bytes.Equal(policy.runtimesBytes, catalog.runtimesBytes) {
		return errors.New("build policy runtimes do not exact-match authenticated catalog")
	}
	registryDigest, err := registry.Digest()
	if err != nil {
		return err
	}
	if registryDigest != policy.toolRegistryDigest {
		return errors.New("build policy toolRegistryDigest does not exact-match authenticated registry")
	}
	for region, target := range policy.current {
		runtime := policy.runtimes[target.RuntimeDigest]
		toolchain, err := registry.Toolchain(target.StandardToolchainDigest)
		if err != nil {
			return fmt.Errorf("build policy current region %q: %w", region, err)
		}
		if toolchain.Architecture != runtime.Architecture ||
			toolchain.ManagedRuntimeDigest != runtime.Digest {
			return fmt.Errorf(
				"build policy current region %q has an incompatible standard toolchain",
				region,
			)
		}
	}
	policy.registry = registry
	return nil
}

func validateBuildPolicyDocument(document buildPolicyDocument) error {
	if document.FormatVersion != BuildPolicyFormatVersion {
		return fmt.Errorf(
			"build policy formatVersion = %d, want %d",
			document.FormatVersion,
			BuildPolicyFormatVersion,
		)
	}
	if document.Current == nil {
		return errors.New("build policy current must be an object")
	}
	if !validToolDigest(document.ToolRegistryDigest) {
		return errors.New("build policy toolRegistryDigest is invalid")
	}
	registered := make(map[string]struct{}, len(document.Runtimes))
	if err := validateRuntimeDescriptors("build policy", document.Runtimes); err != nil {
		return err
	}
	for _, descriptor := range document.Runtimes {
		registered[descriptor.Digest] = struct{}{}
	}
	for region, target := range document.Current {
		if err := regionpkg.ValidateID(region); err != nil {
			return fmt.Errorf("build policy current region %q: %w", region, err)
		}
		if _, ok := registered[target.RuntimeDigest]; !ok {
			return fmt.Errorf(
				"build policy current region %q references unregistered runtime %q",
				region,
				target.RuntimeDigest,
			)
		}
		if !validToolDigest(target.StandardToolchainDigest) {
			return fmt.Errorf(
				"build policy current region %q has an invalid standard toolchain digest",
				region,
			)
		}
		if target.MaterializerVersion != DependencyMaterializerVersion {
			return fmt.Errorf(
				"build policy current region %q materializerVersion = %q, want %q",
				region,
				target.MaterializerVersion,
				DependencyMaterializerVersion,
			)
		}
	}
	return nil
}

func canonicalBuildPolicyDocument(document buildPolicyDocument) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode build policy: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize build policy: %w", err)
	}
	if len(canonical) == 0 || int64(len(canonical)) > maxBuildPolicyBytes {
		return nil, fmt.Errorf(
			"build policy size is outside [1,%d]",
			maxBuildPolicyBytes,
		)
	}
	return canonical, nil
}
