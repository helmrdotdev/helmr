package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	BuildPolicyFormatVersion = 0
	maxBuildPolicyBytes      = 1 << 20

	NodeRuntimeAdapterVersion      = "helmr.runtime.v0"
	ToolchainAdapterVersion        = "helmr.standard-toolchain.v0"
	PlatformFixtureSet             = "helmr.platform.fixtures.v0"
	PlatformDescriptorSchemaV0     = 0
	NodeReleaseOrigin              = "https://nodejs.org/dist/"
	NPMReleaseOrigin               = "https://registry.npmjs.org/npm/"
	PNPMReleaseOrigin              = "https://registry.npmjs.org/pnpm/"
	BunReleaseOrigin               = "https://github.com/oven-sh/bun/releases/"
	BunMetadataOrigin              = "https://api.github.com/repos/oven-sh/bun/releases/tags/"
	PlatformTreeInputMediaType     = "application/vnd.helmr.platform-tree.v0+tar"
	NodeNoStripTypes               = "--no-strip-types"
	NodeNoExperimentalStripTypes   = "--no-experimental-strip-types"
	maxNodeReleaseKeyringBytes     = 1 << 20
	maxBuildPolicyCollectionLength = 256
)

type VersionDomain struct {
	Major   int    `json:"major"`
	Minimum string `json:"minimum"`
}

type NodePolicy struct {
	AdapterVersion         string          `json:"adapterVersion"`
	AllowedOrigin          string          `json:"allowedOrigin"`
	AllowedRedirectHosts   []string        `json:"allowedRedirectHosts"`
	Domains                []VersionDomain `json:"domains"`
	ReleaseKeyFingerprints []string        `json:"releaseKeyFingerprints"`
	ReleaseKeyring         string          `json:"releaseKeyring"`
}

type ManagerPolicy struct {
	AdapterVersion       string             `json:"adapterVersion"`
	AllowedOrigin        string             `json:"allowedOrigin"`
	AllowedRedirectHosts []string           `json:"allowedRedirectHosts"`
	Domain               VersionDomain      `json:"domain"`
	MetadataOrigin       string             `json:"metadataOrigin"`
	Name                 PackageManagerName `json:"name"`
}

type RuntimeInputs struct {
	ConfigEvaluatorDigest string             `json:"configEvaluatorDigest"`
	Harness               ArtifactDescriptor `json:"harness"`
}

type ToolchainInputs struct {
	Base ArtifactDescriptor `json:"base"`
}

type BuildPolicyDenies struct {
	Digests   []string `json:"digests"`
	Selectors []string `json:"selectors"`
}

type buildPolicyDocument struct {
	Architecture            RuntimeArchitecture `json:"architecture"`
	Denies                  BuildPolicyDenies   `json:"denies"`
	DescriptorSchemaVersion int                 `json:"descriptorSchemaVersion"`
	FixtureSet              string              `json:"fixtureSet"`
	FormatVersion           int                 `json:"formatVersion"`
	Managers                []ManagerPolicy     `json:"managers"`
	Node                    NodePolicy          `json:"node"`
	Runtime                 RuntimeInputs       `json:"runtime"`
	Toolchain               ToolchainInputs     `json:"toolchain"`
}

type BuildPolicy struct {
	digest   string
	document buildPolicyDocument
	managers map[PackageManagerName]ManagerPolicy
}

type PlatformAcquisitionPolicy struct {
	DescriptorSchemaVersion int
	FixtureSet              string
	Manager                 ManagerPolicy
	Node                    NodePolicy
	NodeFlag                string
	Runtime                 RuntimeInputs
	Toolchain               ToolchainInputs
}

func LoadBuildPolicy(path string) (*BuildPolicy, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open build policy: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBuildPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read build policy: %w", err)
	}
	return ParseBuildPolicy(raw)
}

func ParseBuildPolicy(raw []byte) (*BuildPolicy, error) {
	if len(raw) == 0 || len(raw) > maxBuildPolicyBytes {
		return nil, fmt.Errorf("build policy size is outside [1,%d]", maxBuildPolicyBytes)
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
	policy := &BuildPolicy{
		digest:   fmt.Sprintf("sha256:%x", sha256.Sum256(raw)),
		document: document,
		managers: make(map[PackageManagerName]ManagerPolicy, len(document.Managers)),
	}
	for _, manager := range document.Managers {
		policy.managers[manager.Name] = manager
	}
	return policy, nil
}

func canonicalBuildPolicy(document buildPolicyDocument) ([]byte, error) {
	if err := validateBuildPolicyDocument(document); err != nil {
		return nil, err
	}
	return canonicalBuildPolicyDocument(document)
}

func ComposeBuildPolicy(
	runtime RuntimeInputs,
	toolchain ToolchainInputs,
	nodeReleaseKeyring []byte,
	nodeReleaseKeyFingerprints []string,
) ([]byte, error) {
	fingerprints := slices.Clone(nodeReleaseKeyFingerprints)
	slices.Sort(fingerprints)
	return canonicalBuildPolicy(buildPolicyDocument{
		Architecture:            ArchitectureX8664,
		Denies:                  BuildPolicyDenies{Digests: []string{}, Selectors: []string{}},
		DescriptorSchemaVersion: PlatformDescriptorSchemaV0,
		FixtureSet:              PlatformFixtureSet,
		FormatVersion:           BuildPolicyFormatVersion,
		Managers: []ManagerPolicy{
			{
				AdapterVersion:       ManagerAdapterVersion,
				AllowedOrigin:        BunReleaseOrigin,
				AllowedRedirectHosts: []string{"api.github.com", "github.com", "objects.githubusercontent.com"},
				Domain:               VersionDomain{Major: 1, Minimum: "1.3.10"},
				MetadataOrigin:       BunMetadataOrigin,
				Name:                 PackageManagerBun,
			},
			{
				AdapterVersion:       ManagerAdapterVersion,
				AllowedOrigin:        NPMReleaseOrigin,
				AllowedRedirectHosts: []string{"registry.npmjs.org"},
				Domain:               VersionDomain{Major: 11, Minimum: "11.4.2"},
				MetadataOrigin:       NPMReleaseOrigin,
				Name:                 PackageManagerNPM,
			},
			{
				AdapterVersion:       ManagerAdapterVersion,
				AllowedOrigin:        PNPMReleaseOrigin,
				AllowedRedirectHosts: []string{"registry.npmjs.org"},
				Domain:               VersionDomain{Major: 11, Minimum: "11.1.0"},
				MetadataOrigin:       PNPMReleaseOrigin,
				Name:                 PackageManagerPNPM,
			},
		},
		Node: NodePolicy{
			AdapterVersion:         NodeRuntimeAdapterVersion,
			AllowedOrigin:          NodeReleaseOrigin,
			AllowedRedirectHosts:   []string{"nodejs.org"},
			Domains:                []VersionDomain{{Major: 22, Minimum: "22.18.0"}, {Major: 24, Minimum: "24.3.0"}},
			ReleaseKeyFingerprints: fingerprints,
			ReleaseKeyring:         base64.StdEncoding.EncodeToString(nodeReleaseKeyring),
		},
		Runtime:   runtime,
		Toolchain: toolchain,
	})
}

func (p *BuildPolicy) Digest() (string, error) {
	if p == nil || !sha256DigestPattern.MatchString(p.digest) {
		return "", errors.New("build policy digest is invalid")
	}
	return p.digest, nil
}

func (p *BuildPolicy) PlatformInputs() (RuntimeInputs, ToolchainInputs, error) {
	if p == nil {
		return RuntimeInputs{}, ToolchainInputs{}, errors.New("build policy is required")
	}
	return p.document.Runtime, p.document.Toolchain, nil
}

func (p *BuildPolicy) Node(version string) (VersionDomain, string, error) {
	if p == nil {
		return VersionDomain{}, "", errors.New("build policy is required")
	}
	major, minor, patch, ok := parseReleaseVersion(version)
	if !ok {
		return VersionDomain{}, "", fmt.Errorf("Node version %q is not an exact release", version)
	}
	for _, domain := range p.document.Node.Domains {
		if major != domain.Major {
			continue
		}
		if compareReleaseVersion(major, minor, patch, domain.Minimum) < 0 {
			break
		}
		flag := NodeNoExperimentalStripTypes
		if major > 24 || major == 24 && compareReleaseVersion(major, minor, patch, "24.12.0") >= 0 {
			flag = NodeNoStripTypes
		}
		return domain, flag, nil
	}
	return VersionDomain{}, "", fmt.Errorf("Node version %q is outside the build policy domains", version)
}

func (p *BuildPolicy) Manager(manager PackageManager) (ManagerPolicy, error) {
	if p == nil {
		return ManagerPolicy{}, errors.New("build policy is required")
	}
	if err := validatePackageManagerSyntax(manager); err != nil {
		return ManagerPolicy{}, err
	}
	domain, ok := p.managers[manager.Name]
	if !ok {
		return ManagerPolicy{}, fmt.Errorf("package manager %q is outside the build policy", manager.Name)
	}
	major, minor, patch, _ := parseReleaseVersion(manager.Version)
	if major != domain.Domain.Major ||
		compareReleaseVersion(major, minor, patch, domain.Domain.Minimum) < 0 {
		return ManagerPolicy{}, fmt.Errorf(
			"%s version %q is outside the build policy domain",
			manager.Name,
			manager.Version,
		)
	}
	return domain, nil
}

func (p *BuildPolicy) Acquisition(
	nodeVersion string,
	manager PackageManager,
) (PlatformAcquisitionPolicy, error) {
	if p == nil {
		return PlatformAcquisitionPolicy{}, errors.New("build policy is required")
	}
	if p.DeniesSelector("node@"+nodeVersion) ||
		p.DeniesSelector(string(manager.Name)+"@"+manager.Version) {
		return PlatformAcquisitionPolicy{}, errors.New("Platform selector is denied")
	}
	_, flag, err := p.Node(nodeVersion)
	if err != nil {
		return PlatformAcquisitionPolicy{}, err
	}
	managerPolicy, err := p.Manager(manager)
	if err != nil {
		return PlatformAcquisitionPolicy{}, err
	}
	return PlatformAcquisitionPolicy{
		DescriptorSchemaVersion: p.document.DescriptorSchemaVersion,
		FixtureSet:              p.document.FixtureSet,
		Manager:                 managerPolicy,
		Node:                    p.document.Node,
		NodeFlag:                flag,
		Runtime:                 p.document.Runtime,
		Toolchain:               p.document.Toolchain,
	}, nil
}

func (p *BuildPolicy) DeniesSelector(selector string) bool {
	return p != nil && slices.Contains(p.document.Denies.Selectors, selector)
}

func (p *BuildPolicy) DeniesDigest(digest string) bool {
	return p != nil && slices.Contains(p.document.Denies.Digests, digest)
}

func validateBuildPolicyDocument(document buildPolicyDocument) error {
	if document.FormatVersion != BuildPolicyFormatVersion {
		return fmt.Errorf(
			"build policy formatVersion = %d, want %d",
			document.FormatVersion,
			BuildPolicyFormatVersion,
		)
	}
	if document.Architecture != ArchitectureX8664 {
		return fmt.Errorf("build policy architecture = %q, want %q", document.Architecture, ArchitectureX8664)
	}
	if document.DescriptorSchemaVersion != PlatformDescriptorSchemaV0 {
		return fmt.Errorf(
			"build policy descriptorSchemaVersion = %d, want %d",
			document.DescriptorSchemaVersion,
			PlatformDescriptorSchemaV0,
		)
	}
	if document.FixtureSet != PlatformFixtureSet {
		return fmt.Errorf("build policy fixtureSet = %q, want %q", document.FixtureSet, PlatformFixtureSet)
	}
	if err := validateNodePolicy(document.Node); err != nil {
		return err
	}
	if err := validateManagerPolicies(document.Managers); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(document.Runtime.ConfigEvaluatorDigest) {
		return errors.New("build policy Runtime input digest is invalid")
	}
	if err := validatePlatformTreeInput(document.Runtime.Harness, "Runtime harness"); err != nil {
		return err
	}
	if err := validatePlatformTreeInput(document.Toolchain.Base, "toolchain base"); err != nil {
		return err
	}
	if err := validateBuildPolicyDenies(document.Denies); err != nil {
		return err
	}
	return nil
}

func validateNodePolicy(policy NodePolicy) error {
	if policy.AdapterVersion != NodeRuntimeAdapterVersion {
		return fmt.Errorf("Node adapterVersion = %q, want %q", policy.AdapterVersion, NodeRuntimeAdapterVersion)
	}
	if policy.AllowedOrigin != NodeReleaseOrigin {
		return fmt.Errorf("Node allowedOrigin = %q, want %q", policy.AllowedOrigin, NodeReleaseOrigin)
	}
	if err := validateSortedStrings(policy.AllowedRedirectHosts, "Node allowedRedirectHosts", false); err != nil {
		return err
	}
	if len(policy.Domains) == 0 || len(policy.Domains) > maxBuildPolicyCollectionLength {
		return errors.New("Node domains are empty or excessive")
	}
	expectedDomains := []VersionDomain{
		{Major: 22, Minimum: "22.18.0"},
		{Major: 24, Minimum: "24.3.0"},
	}
	if !slices.Equal(policy.Domains, expectedDomains) {
		return errors.New("Node domains do not match the v0 adapter contract")
	}
	previousMajor := 0
	for _, domain := range policy.Domains {
		if domain.Major <= previousMajor {
			return errors.New("Node domains are not in ascending unique major order")
		}
		major, _, _, ok := parseReleaseVersion(domain.Minimum)
		if !ok || major != domain.Major {
			return fmt.Errorf("Node domain minimum %q does not match major %d", domain.Minimum, domain.Major)
		}
		previousMajor = domain.Major
	}
	if err := validateSortedStrings(policy.ReleaseKeyFingerprints, "Node releaseKeyFingerprints", false); err != nil {
		return err
	}
	keyring, err := base64.StdEncoding.Strict().DecodeString(policy.ReleaseKeyring)
	if err != nil || len(keyring) == 0 || len(keyring) > maxNodeReleaseKeyringBytes {
		return errors.New("Node releaseKeyring is not bounded canonical base64")
	}
	if base64.StdEncoding.EncodeToString(keyring) != policy.ReleaseKeyring {
		return errors.New("Node releaseKeyring is not canonical base64")
	}
	return nil
}

func validateManagerPolicies(policies []ManagerPolicy) error {
	if len(policies) != 3 {
		return errors.New("build policy must define npm, pnpm, and Bun")
	}
	expected := []struct {
		name           PackageManagerName
		origin         string
		metadataOrigin string
		domain         VersionDomain
	}{
		{PackageManagerBun, BunReleaseOrigin, BunMetadataOrigin, VersionDomain{Major: 1, Minimum: "1.3.10"}},
		{PackageManagerNPM, NPMReleaseOrigin, NPMReleaseOrigin, VersionDomain{Major: 11, Minimum: "11.4.2"}},
		{PackageManagerPNPM, PNPMReleaseOrigin, PNPMReleaseOrigin, VersionDomain{Major: 11, Minimum: "11.1.0"}},
	}
	for index, policy := range policies {
		if policy.Name != expected[index].name {
			return errors.New("build policy managers are not in canonical family order")
		}
		if policy.AdapterVersion != ManagerAdapterVersion {
			return fmt.Errorf("%s adapterVersion = %q, want %q", policy.Name, policy.AdapterVersion, ManagerAdapterVersion)
		}
		if policy.AllowedOrigin != expected[index].origin {
			return fmt.Errorf("%s allowedOrigin = %q, want %q", policy.Name, policy.AllowedOrigin, expected[index].origin)
		}
		if policy.MetadataOrigin != expected[index].metadataOrigin {
			return fmt.Errorf("%s metadataOrigin = %q, want %q", policy.Name, policy.MetadataOrigin, expected[index].metadataOrigin)
		}
		if err := validateSortedStrings(
			policy.AllowedRedirectHosts,
			string(policy.Name)+" allowedRedirectHosts",
			false,
		); err != nil {
			return err
		}
		if policy.Domain != expected[index].domain {
			return fmt.Errorf("%s domain does not match the v0 adapter contract", policy.Name)
		}
	}
	return nil
}

func validatePlatformTreeInput(input ArtifactDescriptor, label string) error {
	if !sha256DigestPattern.MatchString(input.Digest) {
		return fmt.Errorf("%s digest is invalid", label)
	}
	if input.SizeBytes < 1 || input.SizeBytes > maxRuntimePhysicalBytes {
		return fmt.Errorf("%s size is invalid", label)
	}
	if input.MediaType != PlatformTreeInputMediaType {
		return fmt.Errorf("%s mediaType = %q, want %q", label, input.MediaType, PlatformTreeInputMediaType)
	}
	return nil
}

func validateBuildPolicyDenies(denies BuildPolicyDenies) error {
	if err := validateSortedStrings(denies.Digests, "build policy deny digests", true); err != nil {
		return err
	}
	for _, digest := range denies.Digests {
		if !sha256DigestPattern.MatchString(digest) {
			return fmt.Errorf("build policy deny digest %q is invalid", digest)
		}
	}
	return validateSortedStrings(denies.Selectors, "build policy deny selectors", true)
}

func validateSortedStrings(values []string, label string, allowEmpty bool) error {
	if values == nil || len(values) > maxBuildPolicyCollectionLength || !allowEmpty && len(values) == 0 {
		return fmt.Errorf("%s is nil, empty, or excessive", label)
	}
	for index, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("%s contains an invalid value", label)
		}
		if index > 0 && values[index-1] >= value {
			return fmt.Errorf("%s is not sorted and unique", label)
		}
	}
	return nil
}

func validatePackageManagerSyntax(manager PackageManager) error {
	if manager.Name != PackageManagerBun &&
		manager.Name != PackageManagerNPM &&
		manager.Name != PackageManagerPNPM {
		return fmt.Errorf("package manager name %q is unsupported", manager.Name)
	}
	if len(manager.Version) == 0 ||
		len(manager.Version) > maxPackageManagerVersionBytes {
		return fmt.Errorf("package manager version %q is not an admitted SemVer", manager.Version)
	}
	if _, _, _, ok := parseReleaseVersion(manager.Version); !ok {
		return fmt.Errorf("package manager version %q is not an exact release", manager.Version)
	}
	return nil
}

func compareReleaseVersion(major, minor, patch int, minimum string) int {
	var minimumMajor, minimumMinor, minimumPatch int
	_, _ = fmt.Sscanf(minimum, "%d.%d.%d", &minimumMajor, &minimumMinor, &minimumPatch)
	switch {
	case major != minimumMajor:
		return major - minimumMajor
	case minor != minimumMinor:
		return minor - minimumMinor
	default:
		return patch - minimumPatch
	}
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
	if len(canonical) == 0 || len(canonical) > maxBuildPolicyBytes {
		return nil, fmt.Errorf("build policy size is outside [1,%d]", maxBuildPolicyBytes)
	}
	return canonical, nil
}
