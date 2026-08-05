package deployment

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	PlatformDescriptorPath  = "helmr/descriptor.json"
	PlatformIntegrityPath   = "helmr/integrity.json"
	PlatformConformancePath = "helmr/conformance.json"

	PlatformArtifactDocumentFormatVersion = 0
	maxPlatformArtifactDocumentBytes      = 1 << 20
	maxPlatformEvidenceFiles              = 32
	maxPlatformConformanceResults         = 64
)

type PlatformSource struct {
	Digest    string `json:"digest"`
	Origin    string `json:"origin"`
	SizeBytes int64  `json:"sizeBytes"`
}

type RuntimeArtifactDescriptor struct {
	AdapterVersion          string              `json:"adapterVersion"`
	Architecture            RuntimeArchitecture `json:"architecture"`
	ConformanceDigest       string              `json:"conformanceDigest"`
	DescriptorSchemaVersion int                 `json:"descriptorSchemaVersion"`
	Entrypoint              string              `json:"entrypoint"`
	IntegrityDigest         string              `json:"integrityDigest"`
	Kind                    string              `json:"kind"`
	MediaType               string              `json:"mediaType"`
	NodeModuleABI           string              `json:"nodeModuleAbi"`
	NodeVersion             string              `json:"nodeVersion"`
	ProgramNodeFlags        []string            `json:"programNodeFlags"`
	RuntimeContract         string              `json:"runtimeContract"`
	RuntimeHarnessDigest    string              `json:"runtimeHarnessDigest"`
	Source                  PlatformSource      `json:"source"`
}

type ManagerArtifactDescriptor struct {
	AdapterVersion          string              `json:"adapterVersion"`
	Architecture            RuntimeArchitecture `json:"architecture"`
	ConformanceDigest       string              `json:"conformanceDigest"`
	DescriptorSchemaVersion int                 `json:"descriptorSchemaVersion"`
	Entrypoint              ManagerEntrypoint   `json:"entrypoint"`
	IntegrityDigest         string              `json:"integrityDigest"`
	Kind                    string              `json:"kind"`
	MediaType               string              `json:"mediaType"`
	PackageManager          PackageManager      `json:"packageManager"`
	Source                  PlatformSource      `json:"source"`
}

type ToolchainArtifactDescriptor struct {
	AdapterVersion          string              `json:"adapterVersion"`
	Architecture            RuntimeArchitecture `json:"architecture"`
	BaseDigest              string              `json:"baseDigest"`
	Compiler                CompilerInputs      `json:"compiler"`
	ConformanceDigest       string              `json:"conformanceDigest"`
	DescriptorSchemaVersion int                 `json:"descriptorSchemaVersion"`
	IntegrityDigest         string              `json:"integrityDigest"`
	Kind                    string              `json:"kind"`
	MediaType               string              `json:"mediaType"`
	NodeHeadersDigest       string              `json:"nodeHeadersDigest"`
	NodeModuleABI           string              `json:"nodeModuleAbi"`
	NodeSource              PlatformSource      `json:"nodeSource"`
	NodeVersion             string              `json:"nodeVersion"`
	RuntimeDigest           string              `json:"runtimeDigest"`
}

type PlatformEvidenceFile struct {
	Digest    string `json:"digest"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

type PlatformIntegrity struct {
	Evidence      []PlatformEvidenceFile `json:"evidence"`
	FormatVersion int                    `json:"formatVersion"`
	Identity      string                 `json:"identity"`
	IntegrityKind string                 `json:"integrityKind"`
	Redirects     []string               `json:"redirects"`
	Source        PlatformSource         `json:"source"`
}

type PlatformConformanceResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

type PlatformConformance struct {
	ConformanceSet string                      `json:"conformanceSet"`
	FormatVersion  int                         `json:"formatVersion"`
	Inputs         []PlatformEvidenceFile      `json:"inputs"`
	Results        []PlatformConformanceResult `json:"results"`
}

type PlatformArtifactExpectation struct {
	AllowedRedirectHosts    []string
	DescriptorSchemaVersion int
	ConformanceSet          string
	IntegrityIdentities     []string
	IntegrityKind           string
	Manager                 *PackageManager
	NodeVersion             string
	NodeReleaseKeyring      string
	RuntimeDigest           string
	RuntimeHarnessDigest    string
	RequiredConformance     []string
	Compiler                CompilerInputs
	SourceOrigin            string
	ToolchainBaseDigest     string
}

type PlatformArtifactExpectations struct {
	Manager   PlatformArtifactExpectation
	Runtime   PlatformArtifactExpectation
	Toolchain PlatformArtifactExpectation
}

type InspectedPlatformArtifact struct {
	Runtime     *RuntimeArtifactDescriptor
	Manager     *ManagerArtifactDescriptor
	Toolchain   *ToolchainArtifactDescriptor
	Integrity   PlatformIntegrity
	Conformance PlatformConformance
}

func (p *BuildPolicy) PlatformArtifactExpectations(
	nodeVersion string,
	manager PackageManager,
	runtimeDigest string,
) (PlatformArtifactExpectations, error) {
	policy, err := p.Acquisition(nodeVersion, manager)
	if err != nil {
		return PlatformArtifactExpectations{}, err
	}
	managerOrigin, err := ManagerSourceOrigin(manager)
	if err != nil {
		return PlatformArtifactExpectations{}, err
	}
	managerIdentity := "npm-registry"
	managerIntegrity := "ssri-sha512"
	if manager.Name == PackageManagerBun {
		managerIdentity = "github-releases"
		managerIntegrity = "github-sha256"
	}
	return PlatformArtifactExpectations{
		Runtime: PlatformArtifactExpectation{
			AllowedRedirectHosts:    policy.Node.AllowedRedirectHosts,
			DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
			ConformanceSet:          policy.ConformanceSet,
			IntegrityIdentities:     slices.Clone(policy.Node.ReleaseKeyFingerprints),
			IntegrityKind:           "openpgp-sha256",
			NodeVersion:             nodeVersion,
			NodeReleaseKeyring:      policy.Node.ReleaseKeyring,
			RuntimeHarnessDigest:    policy.Runtime.Harness.Digest,
			RequiredConformance:     runtimeConformanceNames(),
			SourceOrigin: NodeReleaseOrigin +
				"v" + nodeVersion +
				"/node-v" + nodeVersion + "-linux-x64.tar.xz",
		},
		Manager: PlatformArtifactExpectation{
			AllowedRedirectHosts:    policy.Manager.AllowedRedirectHosts,
			DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
			ConformanceSet:          policy.ConformanceSet,
			IntegrityIdentities:     []string{managerIdentity},
			IntegrityKind:           managerIntegrity,
			Manager:                 &manager,
			RequiredConformance:     managerConformanceNames(manager.Name),
			SourceOrigin:            managerOrigin,
		},
		Toolchain: PlatformArtifactExpectation{
			Compiler:                policy.Toolchain.Compiler,
			DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
			ConformanceSet:          policy.ConformanceSet,
			IntegrityIdentities:     []string{"helmr-platform"},
			IntegrityKind:           "composed-sha256",
			NodeVersion:             nodeVersion,
			RuntimeDigest:           runtimeDigest,
			RequiredConformance:     toolchainConformanceNames(),
			SourceOrigin:            "platform-cas:" + policy.Toolchain.Base.Digest,
			ToolchainBaseDigest:     policy.Toolchain.Base.Digest,
		},
	}, nil
}

func runtimeConformanceNames() []string {
	return []string{
		"network-denied",
		"node-architecture",
		"node-disable-types",
		"node-module-abi",
		"node-reported-version",
		"runtime-entrypoint",
	}
}

func managerConformanceNames(name PackageManagerName) []string {
	switch name {
	case PackageManagerNPM, PackageManagerBun:
		return []string{
			"entrypoint",
			"reported-version",
			"required-options",
		}
	case PackageManagerPNPM:
		return []string{
			"entrypoint",
			"pnpm-manager-replacement-denied",
			"pnpm-runtime-replacement-denied",
			"reported-version",
			"required-options",
		}
	default:
		panic("validated Manager family is unsupported")
	}
}

func toolchainConformanceNames() []string {
	return []string{
		"compiler-aggregate",
		"compiler-config",
		"compiler-final-modules",
		"compiler-options",
		"esbuild-api",
		"esbuild-binary",
		"native-addon",
		"network-denied",
		"node-headers",
		"runtime-binding",
	}
}

func InspectPlatformArtifact(
	ctx context.Context,
	source io.ReaderAt,
	descriptor ArtifactDescriptor,
	expectation PlatformArtifactExpectation,
) (InspectedPlatformArtifact, error) {
	role, maxLogical, err := platformArtifactRole(descriptor.MediaType)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	spec, err := artifactSnapshotSpecForRole(role)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	if err := validateArtifactSnapshotDescriptor(
		spec,
		artifactSnapshotDescriptor{
			Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
		},
	); err != nil {
		return InspectedPlatformArtifact{}, err
	}
	reader, err := newSquashFSArtifactReader(ctx, source, descriptor.SizeBytes, role)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	artifact, err := inspectArtifact(ctx, reader, role, maxLogical, descriptor.SizeBytes)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	return inspectPlatformArtifact(ctx, artifact, descriptor, expectation)
}

func inspectPlatformArtifact(
	ctx context.Context,
	artifact *inspectedArtifact,
	object ArtifactDescriptor,
	expectation PlatformArtifactExpectation,
) (InspectedPlatformArtifact, error) {
	for _, required := range []string{
		".", "helmr", "helmr/upstream",
		PlatformDescriptorPath, PlatformIntegrityPath, PlatformConformancePath,
	} {
		kind := artifactEntryRegular
		if required == "." || required == "helmr" || required == "helmr/upstream" {
			kind = artifactEntryDirectory
		}
		if _, err := artifact.require(required, kind); err != nil {
			return InspectedPlatformArtifact{}, fmt.Errorf("platform artifact layout: %w", err)
		}
	}
	for _, entry := range artifact.ordered {
		if entry.Path == "helmr" || !strings.HasPrefix(entry.Path, "helmr/") {
			continue
		}
		switch entry.Path {
		case PlatformDescriptorPath, PlatformIntegrityPath, PlatformConformancePath, "helmr/upstream":
		case runtimeEntryPath:
			if object.MediaType != RuntimeArtifactMediaType {
				return InspectedPlatformArtifact{}, fmt.Errorf("unknown platform-owned path %q", entry.Path)
			}
		case "helmr/config-evaluator.mjs", "helmr/esbuild", "helmr/program-compiler.mjs":
			if object.MediaType != ToolchainMediaType {
				return InspectedPlatformArtifact{}, fmt.Errorf("unknown platform-owned path %q", entry.Path)
			}
		default:
			if !strings.HasPrefix(entry.Path, "helmr/upstream/") {
				return InspectedPlatformArtifact{}, fmt.Errorf("unknown platform-owned path %q", entry.Path)
			}
		}
	}
	integrity, integrityRaw, err := readPlatformIntegrity(ctx, artifact)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	conformance, conformanceRaw, err := readPlatformConformance(ctx, artifact)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	if conformance.ConformanceSet != expectation.ConformanceSet {
		return InspectedPlatformArtifact{}, errors.New(
			"platform conformance check set does not match policy",
		)
	}
	if len(conformance.Results) != len(expectation.RequiredConformance) {
		return InspectedPlatformArtifact{}, errors.New("platform conformance result set does not match policy")
	}
	if !slices.Equal(conformance.Inputs, integrity.Evidence) {
		return InspectedPlatformArtifact{}, errors.New("platform conformance inputs differ from integrity evidence")
	}
	for index, result := range conformance.Results {
		if result.Name != expectation.RequiredConformance[index] {
			return InspectedPlatformArtifact{}, errors.New("platform conformance result set does not match policy")
		}
	}
	if err := verifyPlatformEvidence(ctx, artifact, integrity.Evidence); err != nil {
		return InspectedPlatformArtifact{}, err
	}
	if err := verifyPlatformEvidence(ctx, artifact, conformance.Inputs); err != nil {
		return InspectedPlatformArtifact{}, err
	}
	if err := validatePlatformIntegrity(artifact, integrity, expectation); err != nil {
		return InspectedPlatformArtifact{}, err
	}
	if err := verifyUpstreamAuthority(ctx, artifact, object.MediaType, integrity, expectation); err != nil {
		return InspectedPlatformArtifact{}, err
	}
	descriptorRaw, err := artifact.read(ctx, PlatformDescriptorPath, maxPlatformArtifactDocumentBytes)
	if err != nil {
		return InspectedPlatformArtifact{}, err
	}
	integrityDigest := digestDocument(integrityRaw)
	conformanceDigest := digestDocument(conformanceRaw)
	inspected := InspectedPlatformArtifact{Integrity: integrity, Conformance: conformance}
	switch object.MediaType {
	case RuntimeArtifactMediaType:
		var value RuntimeArtifactDescriptor
		if err := parsePlatformDocument(descriptorRaw, "runtime descriptor", &value); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if err := validateRuntimeArtifactDescriptor(value, expectation, integrityDigest, conformanceDigest); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if value.Source != integrity.Source {
			return InspectedPlatformArtifact{}, errors.New("runtime descriptor and integrity source differ")
		}
		inspected.Runtime = &value
	case ManagerTreeMediaType:
		var value ManagerArtifactDescriptor
		if err := parsePlatformDocument(descriptorRaw, "manager descriptor", &value); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if err := validateManagerArtifactDescriptor(value, expectation, integrityDigest, conformanceDigest); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if value.Source != integrity.Source {
			return InspectedPlatformArtifact{}, errors.New("manager descriptor and integrity source differ")
		}
		inspected.Manager = &value
	case ToolchainMediaType:
		var value ToolchainArtifactDescriptor
		if err := parsePlatformDocument(descriptorRaw, "toolchain descriptor", &value); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if err := validateToolchainArtifactDescriptor(value, expectation, integrityDigest, conformanceDigest); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		if err := verifyToolchainCompiler(ctx, artifact, value.Compiler); err != nil {
			return InspectedPlatformArtifact{}, err
		}
		inspected.Toolchain = &value
	}
	return inspected, nil
}

func CanonicalPlatformDocument(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 || len(canonical) > maxPlatformArtifactDocumentBytes {
		return nil, errors.New("platform artifact document is empty or excessive")
	}
	return canonical, nil
}

func readPlatformIntegrity(
	ctx context.Context,
	artifact *inspectedArtifact,
) (PlatformIntegrity, []byte, error) {
	raw, err := artifact.read(ctx, PlatformIntegrityPath, maxPlatformArtifactDocumentBytes)
	if err != nil {
		return PlatformIntegrity{}, nil, err
	}
	var value PlatformIntegrity
	if err := parsePlatformDocument(raw, "Platform integrity", &value); err != nil {
		return PlatformIntegrity{}, nil, err
	}
	if value.FormatVersion != PlatformArtifactDocumentFormatVersion ||
		value.Source.Origin == "" ||
		!sha256DigestPattern.MatchString(value.Source.Digest) ||
		value.Source.SizeBytes < 1 ||
		value.IntegrityKind == "" ||
		value.Identity == "" ||
		len(value.Evidence) == 0 ||
		len(value.Evidence) > maxPlatformEvidenceFiles {
		return PlatformIntegrity{}, nil, errors.New("platform integrity document is invalid")
	}
	return value, raw, nil
}

func readPlatformConformance(
	ctx context.Context,
	artifact *inspectedArtifact,
) (PlatformConformance, []byte, error) {
	raw, err := artifact.read(ctx, PlatformConformancePath, maxPlatformArtifactDocumentBytes)
	if err != nil {
		return PlatformConformance{}, nil, err
	}
	var value PlatformConformance
	if err := parsePlatformDocument(raw, "Platform conformance", &value); err != nil {
		return PlatformConformance{}, nil, err
	}
	if value.FormatVersion != PlatformArtifactDocumentFormatVersion ||
		value.ConformanceSet == "" ||
		len(value.Results) == 0 ||
		len(value.Results) > maxPlatformConformanceResults {
		return PlatformConformance{}, nil, errors.New("platform conformance document is invalid")
	}
	previous := ""
	for _, result := range value.Results {
		if result.Name <= previous || result.Outcome != "passed" {
			return PlatformConformance{}, nil, errors.New("platform conformance results are not sorted passed results")
		}
		previous = result.Name
	}
	return value, raw, nil
}

func verifyPlatformEvidence(
	ctx context.Context,
	artifact *inspectedArtifact,
	files []PlatformEvidenceFile,
) error {
	if len(files) > maxPlatformEvidenceFiles {
		return errors.New("platform evidence file count is excessive")
	}
	previous := ""
	for _, file := range files {
		if file.Path <= previous ||
			!strings.HasPrefix(file.Path, "helmr/upstream/") ||
			path.Clean(file.Path) != file.Path ||
			!sha256DigestPattern.MatchString(file.Digest) ||
			file.SizeBytes < 1 ||
			file.SizeBytes > maxArtifactFileSize {
			return errors.New("platform evidence file descriptor is invalid")
		}
		reader, err := artifact.reader.Open(ctx, file.Path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		size, copyErr := copyExact(ctx, hash, reader, file.SizeBytes)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if size != file.SizeBytes ||
			"sha256:"+hex.EncodeToString(hash.Sum(nil)) != file.Digest {
			return fmt.Errorf("platform evidence file %q does not match its descriptor", file.Path)
		}
		previous = file.Path
	}
	return nil
}

func verifyUpstreamAuthority(
	ctx context.Context,
	artifact *inspectedArtifact,
	mediaType string,
	integrity PlatformIntegrity,
	expectation PlatformArtifactExpectation,
) error {
	if mediaType == ToolchainMediaType {
		return requirePlatformEvidencePaths(
			integrity.Evidence,
			"helmr/upstream/toolchain-inputs.json",
		)
	}
	if mediaType == RuntimeArtifactMediaType && expectation.NodeReleaseKeyring == "" {
		return nil
	}
	if mediaType == ManagerTreeMediaType && expectation.Manager == nil {
		return nil
	}
	if err := validateRetainedSourceEvidence(integrity); err != nil {
		return err
	}
	source, err := artifact.reader.Open(ctx, "helmr/upstream/source")
	if err != nil {
		return fmt.Errorf("open retained upstream source: %w", err)
	}
	defer source.Close()
	switch mediaType {
	case RuntimeArtifactMediaType:
		return verifyRetainedNodeSource(ctx, artifact, source, integrity, expectation)
	case ManagerTreeMediaType:
		return verifyRetainedManagerSource(ctx, artifact, source, integrity, expectation)
	default:
		return errors.New("platform artifact media type has no upstream authority verifier")
	}
}

func validateRetainedSourceEvidence(integrity PlatformIntegrity) error {
	for _, evidence := range integrity.Evidence {
		if evidence.Path == "helmr/upstream/source" &&
			evidence.Digest == integrity.Source.Digest &&
			evidence.SizeBytes == integrity.Source.SizeBytes {
			return nil
		}
	}
	return errors.New("retained upstream source does not match integrity source")
}

func verifyRetainedNodeSource(
	ctx context.Context,
	artifact *inspectedArtifact,
	source io.Reader,
	integrity PlatformIntegrity,
	expectation PlatformArtifactExpectation,
) error {
	if err := requirePlatformEvidencePaths(
		integrity.Evidence,
		"helmr/upstream/SHASUMS256.txt",
		"helmr/upstream/SHASUMS256.txt.asc",
		"helmr/upstream/runtime-inputs.json",
		"helmr/upstream/source",
	); err != nil {
		return err
	}
	signed, err := artifact.read(ctx, "helmr/upstream/SHASUMS256.txt.asc", maxUpstreamMetadataBytes)
	if err != nil {
		return err
	}
	plain, err := artifact.read(ctx, "helmr/upstream/SHASUMS256.txt", maxUpstreamMetadataBytes)
	if err != nil {
		return err
	}
	keyringRaw, err := base64.StdEncoding.Strict().DecodeString(expectation.NodeReleaseKeyring)
	if err != nil || len(keyringRaw) == 0 || len(keyringRaw) > maxNodeReleaseKeyringBytes {
		return errors.New("the Node.js release keyring is invalid")
	}
	keyring, err := openpgp.ReadKeyRing(bytes.NewReader(keyringRaw))
	if err != nil {
		return fmt.Errorf("read Node.js release keyring: %w", err)
	}
	block, rest := clearsign.Decode(signed)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 ||
		block.ArmoredSignature == nil ||
		!bytes.Equal(block.Plaintext, plain) {
		return errors.New("retained Node.js checksum signature is invalid")
	}
	signature, signer, err := openpgp.VerifyDetachedSignature(
		keyring,
		bytes.NewReader(block.Bytes),
		block.ArmoredSignature.Body,
		nil,
	)
	if err != nil || signature == nil || signer == nil ||
		len(signature.IssuerFingerprint) == 0 {
		return errors.New("retained Node.js checksum signature is not valid")
	}
	fingerprint := strings.ToUpper(hex.EncodeToString(signature.IssuerFingerprint))
	if fingerprint != integrity.Identity ||
		!slices.Contains(expectation.IntegrityIdentities, fingerprint) {
		return errors.New("retained Node.js checksum signer is not allowed")
	}
	filename := path.Base(expectation.SourceOrigin)
	want, err := nodeChecksum(plain, filename)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := copyExact(ctx, hash, source, integrity.Source.SizeBytes)
	if err != nil {
		return err
	}
	if size != integrity.Source.SizeBytes ||
		hex.EncodeToString(hash.Sum(nil)) != want {
		return errors.New("retained Node.js distribution does not match its signed checksum")
	}
	return nil
}

func verifyRetainedManagerSource(
	ctx context.Context,
	artifact *inspectedArtifact,
	source io.Reader,
	integrity PlatformIntegrity,
	expectation PlatformArtifactExpectation,
) error {
	if expectation.Manager == nil {
		return errors.New("manager expectation is missing")
	}
	manager := *expectation.Manager
	if manager.Name == PackageManagerBun {
		return verifyRetainedBunSource(ctx, artifact, source, integrity, manager)
	}
	metadataRaw, err := artifact.read(ctx, "helmr/upstream/registry-version.json", maxUpstreamMetadataBytes)
	if err != nil {
		return err
	}
	var version registryVersion
	if err := decodeUpstreamJSON(metadataRaw, &version); err != nil {
		return fmt.Errorf("decode retained registry version metadata: %w", err)
	}
	if version.Name != string(manager.Name) ||
		version.Version != manager.Version ||
		version.Dist.Tarball != expectation.SourceOrigin {
		return errors.New("retained registry metadata does not match manager selector")
	}
	if len(version.Dist.Signatures) != 0 {
		keysRaw, err := artifact.read(ctx, "helmr/upstream/registry-keys.json", maxUpstreamMetadataBytes)
		if err != nil {
			return err
		}
		if err := verifyRegistrySignatures(manager, version, keysRaw); err != nil {
			return err
		}
	}
	expectedEvidence := []string{
		"helmr/upstream/registry-version.json",
		"helmr/upstream/source",
	}
	if len(version.Dist.Signatures) != 0 {
		expectedEvidence = append(expectedEvidence, "helmr/upstream/registry-keys.json")
	}
	if err := requirePlatformEvidencePaths(integrity.Evidence, expectedEvidence...); err != nil {
		return err
	}
	return verifyRetainedRegistryDistribution(
		ctx,
		source,
		integrity.Source.SizeBytes,
		version.Dist.Integrity,
		manager.Integrity,
	)
}

func verifyRetainedRegistryDistribution(
	ctx context.Context,
	source io.Reader,
	size int64,
	ssri string,
	managerIntegrity string,
) error {
	encoded := strings.TrimPrefix(ssri, "sha512-")
	wantSSRI, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(wantSSRI) != sha512.Size ||
		base64.StdEncoding.EncodeToString(wantSSRI) != encoded {
		return errors.New("registry dist.integrity is not a canonical SHA-512 SRI")
	}
	ssriHash := sha512.New()
	writers := []io.Writer{ssriHash}
	var managerHash hash.Hash
	var wantManager string
	if managerIntegrity != "" {
		match := packageManagerIntegrityPattern.FindStringSubmatch(managerIntegrity)
		if match == nil {
			return errors.New("package manager integrity is invalid")
		}
		var algorithm crypto.Hash
		switch match[1] {
		case "sha224":
			algorithm = crypto.SHA224
		case "sha256":
			algorithm = crypto.SHA256
		case "sha384":
			algorithm = crypto.SHA384
		case "sha512":
			algorithm = crypto.SHA512
		}
		managerHash = algorithm.New()
		wantManager = match[2]
		writers = append(writers, managerHash)
	}
	written, err := copyExact(ctx, io.MultiWriter(writers...), source, size)
	if err != nil {
		return err
	}
	if written != size || !bytes.Equal(ssriHash.Sum(nil), wantSSRI) {
		return errors.New("retained manager distribution does not match dist.integrity")
	}
	if managerHash != nil &&
		hex.EncodeToString(managerHash.Sum(nil)) != wantManager {
		return errors.New("retained manager distribution does not match packageManager integrity")
	}
	return nil
}

func requirePlatformEvidencePaths(
	files []PlatformEvidenceFile,
	expected ...string,
) error {
	actual := make([]string, len(files))
	for index, file := range files {
		actual[index] = file.Path
	}
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		return errors.New("platform evidence path set does not match artifact kind")
	}
	return nil
}

func verifyRetainedBunSource(
	ctx context.Context,
	artifact *inspectedArtifact,
	source io.Reader,
	integrity PlatformIntegrity,
	manager PackageManager,
) error {
	if err := requirePlatformEvidencePaths(
		integrity.Evidence,
		"helmr/upstream/github-release.json",
		"helmr/upstream/source",
	); err != nil {
		return err
	}
	metadataRaw, err := artifact.read(ctx, "helmr/upstream/github-release.json", maxUpstreamMetadataBytes)
	if err != nil {
		return err
	}
	var release githubRelease
	if err := decodeUpstreamJSON(metadataRaw, &release); err != nil ||
		release.TagName != "bun-v"+manager.Version {
		return errors.New("retained Bun release metadata does not match manager selector")
	}
	const assetName = "bun-linux-x64-baseline.zip"
	digest := ""
	for _, asset := range release.Assets {
		if asset.Name == assetName &&
			asset.BrowserDownloadURL == integrity.Source.Origin {
			if digest != "" {
				return errors.New("retained Bun release asset is ambiguous")
			}
			digest = asset.Digest
		}
	}
	if digest == "" || digest != integrity.Source.Digest {
		return errors.New("retained Bun release digest does not match source")
	}
	hash := sha256.New()
	size, err := copyExact(ctx, hash, source, integrity.Source.SizeBytes)
	if err != nil {
		return err
	}
	if size != integrity.Source.SizeBytes ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("retained Bun distribution does not match official digest")
	}
	return nil
}

func validatePlatformIntegrity(
	artifact *inspectedArtifact,
	integrity PlatformIntegrity,
	expectation PlatformArtifactExpectation,
) error {
	if integrity.Source.Origin != expectation.SourceOrigin ||
		integrity.IntegrityKind != expectation.IntegrityKind ||
		!slices.Contains(expectation.IntegrityIdentities, integrity.Identity) {
		return errors.New("platform integrity authority does not match policy")
	}
	allowedHosts := make(map[string]struct{}, len(expectation.AllowedRedirectHosts))
	for _, host := range expectation.AllowedRedirectHosts {
		allowedHosts[host] = struct{}{}
	}
	seenRedirects := make(map[string]struct{}, len(integrity.Redirects))
	for _, redirect := range integrity.Redirects {
		if _, exists := seenRedirects[redirect]; exists {
			return errors.New("platform redirect chain contains a duplicate")
		}
		host, ok := httpsURLHost(redirect)
		if !ok {
			return errors.New("platform redirect is not an absolute HTTPS URL")
		}
		if _, ok := allowedHosts[host]; !ok {
			return errors.New("platform redirect escaped its policy hosts")
		}
		seenRedirects[redirect] = struct{}{}
	}
	described := make(map[string]struct{}, len(integrity.Evidence))
	for _, file := range integrity.Evidence {
		described[file.Path] = struct{}{}
	}
	for _, entry := range artifact.ordered {
		if entry.Kind != artifactEntryRegular ||
			!strings.HasPrefix(entry.Path, "helmr/upstream/") {
			continue
		}
		if _, ok := described[entry.Path]; !ok {
			return fmt.Errorf("platform evidence file %q is not described", entry.Path)
		}
	}
	if len(described) != countPlatformEvidenceFiles(artifact) {
		return errors.New("platform evidence document contains a duplicate or missing file")
	}
	return nil
}

func countPlatformEvidenceFiles(artifact *inspectedArtifact) int {
	count := 0
	for _, entry := range artifact.ordered {
		if entry.Kind == artifactEntryRegular &&
			strings.HasPrefix(entry.Path, "helmr/upstream/") {
			count++
		}
	}
	return count
}

func httpsURLHost(raw string) (string, bool) {
	const prefix = "https://"
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(raw, prefix)
	host, suffix, found := strings.Cut(remainder, "/")
	if !found || host == "" || suffix == "" ||
		strings.ContainsAny(host, "@:#?") ||
		strings.ToLower(host) != host {
		return "", false
	}
	return host, true
}

func validateRuntimeArtifactDescriptor(
	value RuntimeArtifactDescriptor,
	expectation PlatformArtifactExpectation,
	integrityDigest,
	conformanceDigest string,
) error {
	programNodeFlags, err := NodeProgramFlags(value.NodeVersion)
	if err != nil {
		return errors.New("runtime descriptor Node.js version has no program launch contract")
	}
	if value.Kind != "runtime" ||
		value.DescriptorSchemaVersion != expectation.DescriptorSchemaVersion ||
		value.AdapterVersion != NodeRuntimeAdapterVersion ||
		value.Architecture != ArchitectureX8664 ||
		value.MediaType != RuntimeArtifactMediaType ||
		value.RuntimeContract != RuntimeContract ||
		value.Entrypoint != "/opt/helmr/runtime/helmr/entry.mjs" ||
		value.NodeVersion != expectation.NodeVersion ||
		value.RuntimeHarnessDigest != expectation.RuntimeHarnessDigest ||
		value.IntegrityDigest != integrityDigest ||
		value.ConformanceDigest != conformanceDigest ||
		!sha256DigestPattern.MatchString(value.RuntimeHarnessDigest) ||
		!sha256DigestPattern.MatchString(value.IntegrityDigest) ||
		!sha256DigestPattern.MatchString(value.ConformanceDigest) ||
		value.NodeModuleABI == "" ||
		!slices.Equal(value.ProgramNodeFlags, programNodeFlags) {
		return errors.New("runtime descriptor does not match acquisition authority")
	}
	return validatePlatformSource(value.Source)
}

func ParseRuntimeArtifactDescriptor(raw []byte) (RuntimeArtifactDescriptor, error) {
	if len(raw) == 0 || len(raw) > maxPlatformArtifactDocumentBytes {
		return RuntimeArtifactDescriptor{}, fmt.Errorf(
			"runtime descriptor size is outside [1,%d]",
			maxPlatformArtifactDocumentBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return RuntimeArtifactDescriptor{}, fmt.Errorf(
			"canonicalize runtime descriptor: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return RuntimeArtifactDescriptor{}, errors.New(
			"runtime descriptor is not RFC 8785 canonical JSON",
		)
	}
	var descriptor RuntimeArtifactDescriptor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return RuntimeArtifactDescriptor{}, fmt.Errorf(
			"decode runtime descriptor: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "runtime descriptor"); err != nil {
		return RuntimeArtifactDescriptor{}, err
	}
	if err := validateRuntimeArtifactDescriptor(
		descriptor,
		PlatformArtifactExpectation{
			DescriptorSchemaVersion: PlatformDescriptorSchemaV0,
			NodeVersion:             descriptor.NodeVersion,
			RuntimeHarnessDigest:    descriptor.RuntimeHarnessDigest,
		},
		descriptor.IntegrityDigest,
		descriptor.ConformanceDigest,
	); err != nil {
		return RuntimeArtifactDescriptor{}, err
	}
	complete, err := CanonicalPlatformDocument(descriptor)
	if err != nil {
		return RuntimeArtifactDescriptor{}, err
	}
	if !bytes.Equal(raw, complete) {
		return RuntimeArtifactDescriptor{}, errors.New(
			"runtime descriptor does not match the complete canonical v0 shape",
		)
	}
	return descriptor, nil
}

func validateManagerArtifactDescriptor(
	value ManagerArtifactDescriptor,
	expectation PlatformArtifactExpectation,
	integrityDigest,
	conformanceDigest string,
) error {
	if expectation.Manager == nil ||
		value.Kind != "manager" ||
		value.DescriptorSchemaVersion != expectation.DescriptorSchemaVersion ||
		value.AdapterVersion != ManagerAdapterVersion ||
		value.Architecture != ArchitectureX8664 ||
		value.MediaType != ManagerTreeMediaType ||
		value.PackageManager != *expectation.Manager ||
		value.IntegrityDigest != integrityDigest ||
		value.ConformanceDigest != conformanceDigest {
		return errors.New("manager descriptor does not match acquisition authority")
	}
	kind, entrypoint, _, err := managerDistribution(value.PackageManager)
	if err != nil || value.Entrypoint.Kind != kind || value.Entrypoint.Path != entrypoint {
		return errors.New("manager descriptor entrypoint does not match its family")
	}
	return validatePlatformSource(value.Source)
}

func validateToolchainArtifactDescriptor(
	value ToolchainArtifactDescriptor,
	expectation PlatformArtifactExpectation,
	integrityDigest,
	conformanceDigest string,
) error {
	if value.Kind != "toolchain" ||
		value.DescriptorSchemaVersion != expectation.DescriptorSchemaVersion ||
		value.AdapterVersion != ToolchainAdapterVersion ||
		value.Architecture != ArchitectureX8664 ||
		value.MediaType != ToolchainMediaType ||
		value.NodeVersion != expectation.NodeVersion ||
		value.RuntimeDigest != expectation.RuntimeDigest ||
		value.BaseDigest != expectation.ToolchainBaseDigest ||
		!reflect.DeepEqual(value.Compiler, expectation.Compiler) ||
		value.IntegrityDigest != integrityDigest ||
		value.ConformanceDigest != conformanceDigest ||
		value.NodeHeadersDigest == "" ||
		value.NodeModuleABI == "" {
		return errors.New("toolchain descriptor does not match acquisition authority")
	}
	if err := validatePlatformSource(value.NodeSource); err != nil {
		return fmt.Errorf("toolchain Node.js source: %w", err)
	}
	return nil
}

func ParseToolchainArtifactDescriptor(
	raw []byte,
) (ToolchainArtifactDescriptor, error) {
	if len(raw) == 0 || len(raw) > maxPlatformArtifactDocumentBytes {
		return ToolchainArtifactDescriptor{}, fmt.Errorf(
			"toolchain descriptor size is outside [1,%d]",
			maxPlatformArtifactDocumentBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ToolchainArtifactDescriptor{}, fmt.Errorf(
			"canonicalize toolchain descriptor: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return ToolchainArtifactDescriptor{}, errors.New(
			"toolchain descriptor is not RFC 8785 canonical JSON",
		)
	}
	var descriptor ToolchainArtifactDescriptor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return ToolchainArtifactDescriptor{}, fmt.Errorf(
			"decode toolchain descriptor: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "Toolchain descriptor"); err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	if err := validateCompilerInputs(descriptor.Compiler); err != nil {
		return ToolchainArtifactDescriptor{}, fmt.Errorf(
			"toolchain compiler: %w",
			err,
		)
	}
	if err := validateToolchainArtifactDescriptor(
		descriptor,
		PlatformArtifactExpectation{
			Compiler:                descriptor.Compiler,
			DescriptorSchemaVersion: descriptor.DescriptorSchemaVersion,
			NodeVersion:             descriptor.NodeVersion,
			RuntimeDigest:           descriptor.RuntimeDigest,
			ToolchainBaseDigest:     descriptor.BaseDigest,
		},
		descriptor.IntegrityDigest,
		descriptor.ConformanceDigest,
	); err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	complete, err := CanonicalPlatformDocument(descriptor)
	if err != nil {
		return ToolchainArtifactDescriptor{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ToolchainArtifactDescriptor{}, errors.New(
			"toolchain descriptor does not match the complete canonical v0 shape",
		)
	}
	return descriptor, nil
}

func verifyToolchainCompiler(
	ctx context.Context,
	artifact *inspectedArtifact,
	compiler CompilerInputs,
) error {
	binaryLink, err := artifact.require("helmr/esbuild", artifactEntrySymlink)
	if err != nil {
		return fmt.Errorf("compiler layout: %w", err)
	}
	if binaryLink.LinkTarget != "../node_modules/@esbuild/linux-x64/bin/esbuild" {
		return errors.New("compiler esbuild binary link does not match policy")
	}
	for path, expectation := range map[string]struct {
		digest string
		mode   uint32
	}{
		"helmr/config-evaluator.mjs": {
			digest: compiler.ConfigEvaluator.Digest,
			mode:   0644,
		},
		"helmr/program-compiler.mjs": {
			digest: compiler.ProgramCompiler.Digest,
			mode:   0644,
		},
		"node_modules/@esbuild/linux-x64/bin/esbuild": {
			digest: compiler.Esbuild.BinaryDigest,
			mode:   0755,
		},
	} {
		entry, err := artifact.require(path, artifactEntryRegular)
		if err != nil {
			return fmt.Errorf("compiler layout: %w", err)
		}
		if entry.Mode != expectation.mode {
			return fmt.Errorf(
				"compiler path %q mode = %#o, want %#o",
				path,
				entry.Mode,
				expectation.mode,
			)
		}
		raw, err := artifact.read(ctx, path, maxArtifactFileSize)
		if err != nil {
			return err
		}
		if digestBytes(raw) != expectation.digest {
			return fmt.Errorf("compiler path %q digest does not match policy", path)
		}
	}
	apiDigest, err := compilerPackageDigest(
		ctx,
		artifact,
		"node_modules/esbuild",
	)
	if err != nil {
		return err
	}
	if apiDigest != compiler.Esbuild.APIPackageDigest {
		return errors.New("esbuild API package digest does not match policy")
	}
	headersDigest, err := artifactDirectoryDigest(ctx, artifact, "include/node")
	if err != nil {
		return err
	}
	descriptorRaw, err := artifact.read(
		ctx,
		PlatformDescriptorPath,
		maxPlatformArtifactDocumentBytes,
	)
	if err != nil {
		return err
	}
	var descriptor ToolchainArtifactDescriptor
	if err := parsePlatformDocument(
		descriptorRaw,
		"toolchain descriptor",
		&descriptor,
	); err != nil {
		return err
	}
	if headersDigest != descriptor.NodeHeadersDigest {
		return errors.New("toolchain Node.js headers digest does not match bytes")
	}
	return nil
}

type compilerPackageFile struct {
	Digest    string `json:"digest"`
	Mode      uint32 `json:"mode"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

func compilerPackageDigest(
	ctx context.Context,
	artifact *inspectedArtifact,
	root string,
) (string, error) {
	if _, err := artifact.require(root, artifactEntryDirectory); err != nil {
		return "", fmt.Errorf("compiler layout: %w", err)
	}
	prefix := root + "/"
	files := make([]compilerPackageFile, 0)
	for _, entry := range artifact.ordered {
		if !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		if entry.Kind == artifactEntryDirectory {
			continue
		}
		if entry.Kind != artifactEntryRegular {
			return "", fmt.Errorf(
				"esbuild API package contains unsupported path %q",
				entry.Path,
			)
		}
		raw, err := artifact.read(ctx, entry.Path, maxArtifactFileSize)
		if err != nil {
			return "", err
		}
		files = append(files, compilerPackageFile{
			Digest:    digestBytes(raw),
			Mode:      entry.Mode,
			Path:      strings.TrimPrefix(entry.Path, prefix),
			SizeBytes: entry.SizeBytes,
		})
	}
	if len(files) == 0 {
		return "", errors.New("esbuild API package is empty")
	}
	raw, err := CanonicalPlatformDocument(files)
	if err != nil {
		return "", err
	}
	return digestBytes(raw), nil
}

func artifactDirectoryDigest(
	ctx context.Context,
	artifact *inspectedArtifact,
	root string,
) (string, error) {
	if _, err := artifact.require(root, artifactEntryDirectory); err != nil {
		return "", err
	}
	hash := sha256.New()
	prefix := root + "/"
	for _, entry := range artifact.ordered {
		if !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		mode := os.FileMode(entry.Mode)
		switch entry.Kind {
		case artifactEntryRegular:
		case artifactEntryDirectory:
			mode |= os.ModeDir
		case artifactEntrySymlink:
			mode |= os.ModeSymlink
		default:
			return "", fmt.Errorf(
				"directory %q contains unsupported path %q",
				root,
				entry.Path,
			)
		}
		if _, err := fmt.Fprintf(
			hash,
			"%s\x00%#o\x00",
			strings.TrimPrefix(entry.Path, prefix),
			mode.Type()|mode.Perm(),
		); err != nil {
			return "", err
		}
		switch entry.Kind {
		case artifactEntryRegular:
			raw, err := artifact.read(ctx, entry.Path, maxArtifactFileSize)
			if err != nil {
				return "", err
			}
			if _, err := hash.Write(raw); err != nil {
				return "", err
			}
		case artifactEntrySymlink:
			if _, err := io.WriteString(hash, entry.LinkTarget); err != nil {
				return "", err
			}
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validatePlatformSource(source PlatformSource) error {
	if source.Origin == "" ||
		!sha256DigestPattern.MatchString(source.Digest) ||
		source.SizeBytes < 1 {
		return errors.New("platform source descriptor is invalid")
	}
	return nil
}

func parsePlatformDocument(raw []byte, name string, destination any) error {
	if len(raw) == 0 || len(raw) > maxPlatformArtifactDocumentBytes {
		return fmt.Errorf("%s is empty or excessive", name)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not canonical JSON", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return ensureEOF(decoder, name)
}

func digestDocument(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func platformArtifactRole(mediaType string) (artifactRole, int64, error) {
	switch mediaType {
	case RuntimeArtifactMediaType:
		return runtimeArtifact, maxRuntimeLogicalBytes, nil
	case ManagerTreeMediaType:
		return managerArtifact, maxManagerTreeBytes, nil
	case ToolchainMediaType:
		return toolchainArtifact, maxToolArtifactBytes, nil
	default:
		return 0, 0, fmt.Errorf("platform artifact media type %q is unsupported", mediaType)
	}
}

func SortedPlatformEvidence(files []PlatformEvidenceFile) []PlatformEvidenceFile {
	result := slices.Clone(files)
	slices.SortFunc(result, func(left, right PlatformEvidenceFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	return result
}
