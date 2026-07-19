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
	ErrBuildRegionNotConfigured       = errors.New("build policy region is not configured")
	ErrRuntimeNotRegistered           = errors.New("runtime is not registered")
	ErrStandardToolchainNotRegistered = errors.New("standard toolchain is not registered")
)

type BuildTarget struct {
	Runtime                 RuntimeDescriptor
	StandardToolchainDigest string
	MaterializerVersion     string
}

type BuildPolicy struct {
	current                map[string]buildPolicyTarget
	runtimes               map[string]RuntimeDescriptor
	runtimesBytes          []byte
	toolchains             map[string]Toolchain
	toolchainsBytes        []byte
	toolchainCatalogDigest string
}

type buildPolicyTarget struct {
	MaterializerVersion     string `json:"materializerVersion"`
	RuntimeDigest           string `json:"runtimeDigest"`
	StandardToolchainDigest string `json:"standardToolchainDigest"`
}

type buildPolicyDocument struct {
	Current       map[string]buildPolicyTarget `json:"current"`
	FormatVersion int                          `json:"formatVersion"`
	Runtimes      []RuntimeDescriptor          `json:"runtimes"`
	Toolchains    []Toolchain                  `json:"toolchains"`
}

func LoadBuildPolicy(
	path string,
	catalog *RuntimeCatalog,
	toolchainCatalog *ToolchainCatalog,
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
	if err := validateBuildPolicyCatalogs(policy, catalog, toolchainCatalog); err != nil {
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
	toolchainsBytes, err := canonicalToolchains(document.Toolchains)
	if err != nil {
		return nil, err
	}
	toolchainCatalogDigest, err := toolchainCatalogDigest(document.Toolchains)
	if err != nil {
		return nil, err
	}
	policy := &BuildPolicy{
		current:                make(map[string]buildPolicyTarget, len(document.Current)),
		runtimes:               make(map[string]RuntimeDescriptor, len(document.Runtimes)),
		runtimesBytes:          runtimesBytes,
		toolchains:             make(map[string]Toolchain, len(document.Toolchains)),
		toolchainsBytes:        toolchainsBytes,
		toolchainCatalogDigest: toolchainCatalogDigest,
	}
	for region, target := range document.Current {
		policy.current[region] = target
	}
	for _, descriptor := range document.Runtimes {
		policy.runtimes[descriptor.Digest] = descriptor
	}
	for _, toolchain := range document.Toolchains {
		digest, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return nil, err
		}
		policy.toolchains[digest] = toolchain
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

func (p *BuildPolicy) ResolveToolchain(digest string) (Toolchain, error) {
	if p == nil {
		return Toolchain{}, fmt.Errorf("%w: %q", ErrStandardToolchainNotRegistered, digest)
	}
	toolchain, ok := p.toolchains[digest]
	if !ok {
		return Toolchain{}, fmt.Errorf("%w: %q", ErrStandardToolchainNotRegistered, digest)
	}
	return toolchain, nil
}

func (p *BuildPolicy) ToolchainCatalogDigest() (string, error) {
	if p == nil || !validToolDigest(p.toolchainCatalogDigest) {
		return "", errToolchainCatalogUnauthenticated
	}
	return p.toolchainCatalogDigest, nil
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
	toolchain, err := p.ResolveToolchain(standardToolchainDigest)
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
	for digest, toolchain := range previous.toolchains {
		replacement, ok := next.toolchains[digest]
		if !ok {
			return fmt.Errorf(
				"build policy upgrade removes registered standard toolchain %q",
				digest,
			)
		}
		if replacement != toolchain {
			return fmt.Errorf(
				"build policy upgrade mutates registered standard toolchain %q",
				digest,
			)
		}
	}
	return nil
}

func validateBuildPolicyCatalogs(
	policy *BuildPolicy,
	runtimeCatalog *RuntimeCatalog,
	toolchainCatalog *ToolchainCatalog,
) error {
	if policy == nil {
		return errors.New("build policy is required")
	}
	if runtimeCatalog == nil || !runtimeCatalog.authenticated {
		return errors.New("authenticated runtime catalog is required")
	}
	if !bytes.Equal(policy.runtimesBytes, runtimeCatalog.runtimesBytes) {
		return errors.New("build policy runtimes do not exact-match authenticated catalog")
	}
	if toolchainCatalog == nil || !toolchainCatalog.authenticated {
		return errToolchainCatalogUnauthenticated
	}
	if !bytes.Equal(policy.toolchainsBytes, toolchainCatalog.toolchainsBytes) {
		return errors.New("build policy toolchains do not exact-match authenticated catalog")
	}
	catalogDigest, err := toolchainCatalog.Digest()
	if err != nil {
		return err
	}
	for region, target := range policy.current {
		runtime := policy.runtimes[target.RuntimeDigest]
		toolchain := policy.toolchains[target.StandardToolchainDigest]
		if toolchain.Architecture != runtime.Architecture ||
			toolchain.ManagedRuntimeDigest != runtime.Digest {
			return fmt.Errorf(
				"build policy current region %q has an incompatible standard toolchain",
				region,
			)
		}
	}
	if policy.toolchainCatalogDigest != catalogDigest {
		return errors.New("build policy toolchain catalog identity does not exact-match authenticated catalog")
	}
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
	registeredRuntimes := make(map[string]RuntimeDescriptor, len(document.Runtimes))
	if err := validateRuntimeDescriptors("build policy", document.Runtimes); err != nil {
		return err
	}
	for _, descriptor := range document.Runtimes {
		registeredRuntimes[descriptor.Digest] = descriptor
	}
	if err := validateToolchains("build policy", document.Toolchains); err != nil {
		return err
	}
	registeredToolchains := make(map[string]Toolchain, len(document.Toolchains))
	for _, toolchain := range document.Toolchains {
		digest, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return err
		}
		registeredToolchains[digest] = toolchain
	}
	for region, target := range document.Current {
		if err := regionpkg.ValidateID(region); err != nil {
			return fmt.Errorf("build policy current region %q: %w", region, err)
		}
		runtime, ok := registeredRuntimes[target.RuntimeDigest]
		if !ok {
			return fmt.Errorf(
				"build policy current region %q references unregistered runtime %q",
				region,
				target.RuntimeDigest,
			)
		}
		toolchain, ok := registeredToolchains[target.StandardToolchainDigest]
		if !ok {
			return fmt.Errorf(
				"build policy current region %q references unregistered standard toolchain %q",
				region,
				target.StandardToolchainDigest,
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
		if toolchain.Architecture != runtime.Architecture ||
			toolchain.ManagedRuntimeDigest != runtime.Digest {
			return fmt.Errorf(
				"build policy current region %q has an incompatible standard toolchain",
				region,
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
