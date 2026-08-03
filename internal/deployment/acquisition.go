package deployment

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	maxPlatformAcquisitionTreeBytes = 4 << 30
	maxPlatformAcquisitionEntries   = 200000
)

type PlatformConformanceValidator interface {
	Runtime(
		context.Context,
		string,
		*platformTree,
		RuntimeArtifactDescriptor,
	) (PlatformConformance, error)
	Manager(
		context.Context,
		string,
		*platformTree,
		*platformTree,
		ManagerArtifactDescriptor,
	) (PlatformConformance, error)
	Toolchain(
		context.Context,
		string,
		*platformTree,
		*platformTree,
		ToolchainArtifactDescriptor,
	) (PlatformConformance, error)
}

type PlatformAcquirer struct {
	Encoder   string
	GPGV      string
	HTTP      *http.Client
	Patchelf  string
	Policy    *BuildPolicy
	Store     cas.ImmutableStore
	Validator PlatformConformanceValidator
	WorkDir   string
	XZ        string
}

type platformAcquisitionError struct {
	cause  error
	reason api.WorkerPlatformAcquisitionFailureReason
}

func (err *platformAcquisitionError) Error() string {
	if err.cause == nil {
		return string(err.reason)
	}
	return err.cause.Error()
}
func (err *platformAcquisitionError) Unwrap() error { return err.cause }
func (err *platformAcquisitionError) PlatformAcquisitionFailureReason() api.WorkerPlatformAcquisitionFailureReason {
	return err.reason
}

func deterministicAcquisitionFailure(
	reason api.WorkerPlatformAcquisitionFailureReason,
	cause error,
) error {
	return &platformAcquisitionError{cause: cause, reason: reason}
}

func conformanceFailure(validationErr error, closeErr error) error {
	if closeErr != nil {
		return errors.Join(validationErr, closeErr)
	}
	var invalid *verifierInvalidError
	if errors.As(validationErr, &invalid) {
		return deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionConformanceFailed,
			validationErr,
		)
	}
	return validationErr
}

func (acquirer PlatformAcquirer) Acquire(
	ctx context.Context,
	request api.WorkerPlatformAcquisition,
) (_ api.WorkerPlatformAcquisitionCandidates, returnErr error) {
	if err := acquirer.validate(); err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	manager := PackageManager{
		Integrity: request.ManagerIntegrity,
		Name:      PackageManagerName(request.ManagerName),
		Version:   request.ManagerVersion,
	}
	policyDigest, err := acquirer.Policy.Digest()
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	if request.BuildPolicyDigest != policyDigest ||
		request.BuildContract != ProgramBuildContractVersion {
		return api.WorkerPlatformAcquisitionCandidates{}, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionUnsupportedSelector,
			errors.New("Platform acquisition request does not match Worker authority"),
		)
	}
	policy, err := acquirer.Policy.Acquisition(request.NodeVersion, manager)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionUnsupportedSelector,
			err,
		)
	}
	leaseRoot, err := os.MkdirTemp(acquirer.WorkDir, ".platform-acquisition-")
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, fmt.Errorf("create Platform acquisition lease: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, os.RemoveAll(leaseRoot))
	}()

	node, err := acquirer.acquireNode(ctx, leaseRoot, request.NodeVersion, policy)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	defer node.Close()
	runtimeTree, err := acquirer.runtimeTree(
		ctx,
		leaseRoot,
		request.DeploymentID,
		request.NodeVersion,
		manager,
		policy,
		node,
	)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	defer runtimeTree.Close()
	runtimeObject, err := runtimeTree.publish(ctx, acquirer.Store)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, fmt.Errorf("publish Runtime candidate: %w", err)
	}
	if acquirer.Policy.DeniesDigest(runtimeObject.Digest) {
		return api.WorkerPlatformAcquisitionCandidates{}, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionDenied,
			errors.New("Runtime candidate digest is denied"),
		)
	}

	managerSource, err := acquirer.acquireManager(ctx, leaseRoot, manager, policy.Manager)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	defer managerSource.Close()
	managerTree, err := acquirer.managerTree(
		ctx,
		leaseRoot,
		request.DeploymentID,
		request.NodeVersion,
		manager,
		policy,
		managerSource,
		runtimeTree,
	)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	defer managerTree.Close()
	managerObject, err := managerTree.publish(ctx, acquirer.Store)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, fmt.Errorf("publish Manager candidate: %w", err)
	}
	if acquirer.Policy.DeniesDigest(managerObject.Digest) {
		return api.WorkerPlatformAcquisitionCandidates{}, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionDenied,
			errors.New("Manager candidate digest is denied"),
		)
	}

	toolchainTree, err := acquirer.toolchainTree(
		ctx,
		leaseRoot,
		request.DeploymentID,
		request.NodeVersion,
		manager,
		policy,
		node,
		runtimeTree,
		runtimeObject.Digest,
	)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, err
	}
	defer toolchainTree.Close()
	toolchainObject, err := toolchainTree.publish(ctx, acquirer.Store)
	if err != nil {
		return api.WorkerPlatformAcquisitionCandidates{}, fmt.Errorf("publish toolchain candidate: %w", err)
	}
	if acquirer.Policy.DeniesDigest(toolchainObject.Digest) {
		return api.WorkerPlatformAcquisitionCandidates{}, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionDenied,
			errors.New("toolchain candidate digest is denied"),
		)
	}
	return api.WorkerPlatformAcquisitionCandidates{
		Runtime:   acquiredCASObject(runtimeObject),
		Manager:   acquiredCASObject(managerObject),
		Toolchain: acquiredCASObject(toolchainObject),
	}, nil
}

func acquiredCASObject(object cas.Object) api.CASObject {
	return api.CASObject{
		Digest:    object.Digest,
		MediaType: object.MediaType,
		SizeBytes: object.SizeBytes,
	}
}

func (acquirer PlatformAcquirer) validate() error {
	switch {
	case acquirer.Policy == nil:
		return errors.New("Platform acquisition policy is required")
	case acquirer.Store == nil:
		return errors.New("Platform Artifact store is required")
	case acquirer.Validator == nil:
		return errors.New("Platform conformance validator is required")
	case acquirer.WorkDir == "" || !filepath.IsAbs(acquirer.WorkDir) ||
		filepath.Clean(acquirer.WorkDir) != acquirer.WorkDir:
		return errors.New("Platform acquisition work directory is invalid")
	}
	for name, executable := range map[string]string{
		"GPG verifier":     acquirer.GPGV,
		"ELF patcher":      acquirer.Patchelf,
		"SquashFS encoder": acquirer.Encoder,
		"XZ decoder":       acquirer.XZ,
	} {
		if executable == "" || !filepath.IsAbs(executable) ||
			filepath.Clean(executable) != executable {
			return fmt.Errorf("%s path is invalid", name)
		}
		info, err := os.Stat(executable)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s path is not an executable regular file", name)
		}
	}
	return nil
}

type nodeAcquisition struct {
	distribution *upstreamObject
	evidence     map[string][]byte
	identity     string
	moduleABI    string
	root         string
}

func (node *nodeAcquisition) Close() error {
	if node == nil {
		return nil
	}
	return node.distribution.Close()
}

func (acquirer PlatformAcquirer) acquireNode(
	ctx context.Context,
	leaseRoot string,
	version string,
	policy PlatformAcquisitionPolicy,
) (_ *nodeAcquisition, returnErr error) {
	base := policy.Node.AllowedOrigin + "v" + version + "/"
	checksums, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		base+"SHASUMS256.txt.asc",
		policy.Node.AllowedRedirectHosts,
		maxUpstreamMetadataBytes,
	)
	if err != nil {
		return nil, err
	}
	defer checksums.Close()
	checksumRaw, err := readUpstream(checksums)
	if err != nil {
		return nil, err
	}
	plain, signer, err := acquirer.verifyNodeChecksums(
		ctx,
		leaseRoot,
		checksums.file,
		policy.Node,
	)
	if err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			err,
		)
	}
	filename := "node-v" + version + "-linux-x64.tar.xz"
	want, err := nodeChecksum(plain, filename)
	if err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			err,
		)
	}
	distribution, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		base+filename,
		policy.Node.AllowedRedirectHosts,
		maxNodeDistributionBytes,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if distribution != nil {
			returnErr = errors.Join(returnErr, distribution.Close())
		}
	}()
	if distribution.source.Digest != "sha256:"+want {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			errors.New("Node distribution does not match the signed checksum"),
		)
	}
	extracted := filepath.Join(leaseRoot, "node-source")
	if err := acquirer.extractXZTar(ctx, distribution.file, extracted); err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			fmt.Errorf("extract Node distribution: %w", err),
		)
	}
	root := filepath.Join(extracted, "node-v"+version+"-linux-x64")
	if err := validateNodeDistribution(root); err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			err,
		)
	}
	moduleABI, err := nodeModuleABI(filepath.Join(root, "include", "node", "node_version.h"))
	if err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			err,
		)
	}
	result := &nodeAcquisition{
		distribution: distribution,
		evidence: map[string][]byte{
			"SHASUMS256.txt":     plain,
			"SHASUMS256.txt.asc": checksumRaw,
		},
		identity:  signer,
		moduleABI: moduleABI,
		root:      root,
	}
	distribution = nil
	return result, nil
}

func (acquirer PlatformAcquirer) verifyNodeChecksums(
	ctx context.Context,
	leaseRoot string,
	signed *os.File,
	policy NodePolicy,
) ([]byte, string, error) {
	keyringRaw, err := base64.StdEncoding.Strict().DecodeString(policy.ReleaseKeyring)
	if err != nil {
		return nil, "", err
	}
	keyring := filepath.Join(leaseRoot, "node-release-keyring.gpg")
	if err := os.WriteFile(keyring, keyringRaw, 0400); err != nil {
		return nil, "", err
	}
	output := filepath.Join(leaseRoot, "SHASUMS256.txt")
	var status bytes.Buffer
	var diagnostic bytes.Buffer
	command := exec.CommandContext(
		ctx,
		acquirer.GPGV,
		"--status-fd", "1",
		"--keyring", keyring,
		"--output", output,
		signed.Name(),
	)
	command.Env = []string{"LC_ALL=C"}
	command.Stdout = &status
	command.Stderr = &diagnostic
	if err := command.Run(); err != nil {
		return nil, "", fmt.Errorf("verify Node release signature: %w: %s", err, diagnostic.String())
	}
	signer := ""
	for _, line := range strings.Split(status.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "[GNUPG:]" && fields[1] == "VALIDSIG" &&
			slices.Contains(policy.ReleaseKeyFingerprints, fields[2]) {
			signer = fields[2]
			break
		}
	}
	if signer == "" {
		return nil, "", errors.New("Node release signature has no allowed valid signer")
	}
	plain, err := os.ReadFile(output)
	if err != nil || len(plain) == 0 || len(plain) > maxUpstreamMetadataBytes {
		return nil, "", errors.New("verified Node checksum document is invalid")
	}
	return plain, signer, nil
}

func nodeChecksum(raw []byte, filename string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	found := ""
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[1] != filename {
			continue
		}
		if found != "" || len(fields[0]) != sha256.Size*2 {
			return "", errors.New("Node checksum entry is not unique")
		}
		if _, err := hex.DecodeString(fields[0]); err != nil ||
			strings.ToLower(fields[0]) != fields[0] {
			return "", errors.New("Node checksum entry is invalid")
		}
		found = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("Node checksum entry is missing")
	}
	return found, nil
}

func (acquirer PlatformAcquirer) extractXZTar(
	ctx context.Context,
	source *os.File,
	destination string,
) error {
	if err := os.Mkdir(destination, 0755); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, acquirer.XZ, "--decompress", "--stdout", source.Name())
	command.Env = []string{"LC_ALL=C"}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	if err := command.Start(); err != nil {
		return err
	}
	_, extractErr := archive.ExtractTarWithStats(stdout, destination, archive.ExtractOptions{
		MaxBytes:   maxPlatformAcquisitionTreeBytes,
		MaxEntries: maxPlatformAcquisitionEntries,
	})
	waitErr := command.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("decode XZ: %w: %s", waitErr, diagnostic.String())
	}
	return errors.Join(extractErr, waitErr)
}

func validateNodeDistribution(root string) error {
	expected := []struct {
		path       string
		executable bool
	}{
		{"bin/node", true},
		{"include/node/node.h", false},
		{"include/node/node_api.h", false},
		{"include/node/node_version.h", false},
		{"LICENSE", false},
	}
	for _, value := range expected {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(value.path)))
		if err != nil || !info.Mode().IsRegular() ||
			value.executable && info.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("Node distribution path %q is invalid", value.path)
		}
	}
	parent := filepath.Dir(root)
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || filepath.Join(parent, entries[0].Name()) != root {
		return errors.New("Node distribution top-level topology is invalid")
	}
	return nil
}

func nodeModuleABI(headerPath string) (string, error) {
	file, err := os.Open(headerPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "#define" &&
			fields[1] == "NODE_MODULE_VERSION" {
			if _, err := strconv.Atoi(fields[2]); err != nil {
				return "", errors.New("Node module ABI is not numeric")
			}
			return fields[2], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("Node module ABI is missing")
}

type managerAcquisition struct {
	distribution *upstreamObject
	evidence     map[string][]byte
	identity     string
	integrity    string
	root         string
}

func (manager *managerAcquisition) Close() error {
	if manager == nil {
		return nil
	}
	return manager.distribution.Close()
}

type registryVersion struct {
	Dist struct {
		Integrity  string `json:"integrity"`
		Signatures []struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		} `json:"signatures"`
		Tarball string `json:"tarball"`
	} `json:"dist"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type registryKeys struct {
	Keys []struct {
		Key   string `json:"key"`
		KeyID string `json:"keyid"`
	} `json:"keys"`
}

func (acquirer PlatformAcquirer) acquireManager(
	ctx context.Context,
	leaseRoot string,
	manager PackageManager,
	policy ManagerPolicy,
) (_ *managerAcquisition, returnErr error) {
	if manager.Name == PackageManagerBun {
		return acquirer.acquireBun(ctx, leaseRoot, manager, policy)
	}
	metadataURL := policy.MetadataOrigin + manager.Version
	metadata, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		metadataURL,
		policy.AllowedRedirectHosts,
		maxUpstreamMetadataBytes,
	)
	if err != nil {
		return nil, err
	}
	defer metadata.Close()
	metadataRaw, err := readUpstream(metadata)
	if err != nil {
		return nil, err
	}
	var version registryVersion
	if err := decodeUpstreamJSON(metadataRaw, &version); err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			fmt.Errorf("decode registry version metadata: %w", err),
		)
	}
	origin, err := ManagerSourceOrigin(manager)
	if err != nil || version.Name != string(manager.Name) ||
		version.Version != manager.Version ||
		version.Dist.Tarball != origin ||
		!strings.HasPrefix(version.Dist.Integrity, "sha512-") {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			errors.New("registry version metadata does not match the exact Manager selector"),
		)
	}
	distribution, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		version.Dist.Tarball,
		policy.AllowedRedirectHosts,
		maxRegistryDistributionBytes,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if distribution != nil {
			returnErr = errors.Join(returnErr, distribution.Close())
		}
	}()
	if err := verifySSRI(distribution.file, version.Dist.Integrity); err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			err,
		)
	}
	if err := verifyPackageManagerIntegrity(distribution.file, manager.Integrity); err != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			err,
		)
	}
	evidence := map[string][]byte{"registry-version.json": metadataRaw}
	if len(version.Dist.Signatures) != 0 {
		keys, err := fetchUpstream(
			ctx,
			acquirer.HTTP,
			leaseRoot,
			"https://registry.npmjs.org/-/npm/v1/keys",
			policy.AllowedRedirectHosts,
			maxUpstreamMetadataBytes,
		)
		if err != nil {
			return nil, err
		}
		keysRaw, readErr := readUpstream(keys)
		closeErr := keys.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		if err := verifyRegistrySignatures(manager, version, keysRaw); err != nil {
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionIntegrityFailed,
				err,
			)
		}
		evidence["registry-keys.json"] = keysRaw
	}
	extracted := filepath.Join(leaseRoot, "manager-source")
	if err := os.Mkdir(extracted, 0755); err != nil {
		return nil, err
	}
	if _, err := distribution.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	compressed, err := gzip.NewReader(distribution.file)
	if err != nil {
		return nil, deterministicAcquisitionFailure(api.WorkerPlatformAcquisitionTopologyFailed, err)
	}
	_, extractErr := archive.ExtractTarWithStats(compressed, extracted, archive.ExtractOptions{
		MaxBytes:   maxManagerTreeBytes,
		MaxEntries: maxPlatformAcquisitionEntries,
	})
	closeErr := compressed.Close()
	if extractErr != nil || closeErr != nil {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			errors.Join(extractErr, closeErr),
		)
	}
	root := filepath.Join(extracted, "package")
	if err := validateRegistryManagerTree(root, manager); err != nil {
		return nil, deterministicAcquisitionFailure(api.WorkerPlatformAcquisitionTopologyFailed, err)
	}
	result := &managerAcquisition{
		distribution: distribution,
		evidence:     evidence,
		identity:     "npm-registry",
		integrity:    "ssri-sha512",
		root:         root,
	}
	distribution = nil
	return result, nil
}

func verifySSRI(file *os.File, integrity string) error {
	encoded := strings.TrimPrefix(integrity, "sha512-")
	want, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(want) != sha512.Size {
		return errors.New("registry dist.integrity is not a canonical SHA-512 SRI")
	}
	if base64.StdEncoding.EncodeToString(want) != encoded {
		return errors.New("registry dist.integrity is not canonical")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha512.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), want) {
		return errors.New("Manager distribution does not match dist.integrity")
	}
	return nil
}

func verifyPackageManagerIntegrity(file *os.File, integrity string) error {
	if integrity == "" {
		return nil
	}
	match := packageManagerIntegrityPattern.FindStringSubmatch(integrity)
	if match == nil {
		return errors.New("package manager integrity is invalid")
	}
	var hash crypto.Hash
	switch match[1] {
	case "sha224":
		hash = crypto.SHA224
	case "sha256":
		hash = crypto.SHA256
	case "sha384":
		hash = crypto.SHA384
	case "sha512":
		hash = crypto.SHA512
	default:
		return errors.New("package manager integrity algorithm is unsupported")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := hash.New()
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != match[2] {
		return errors.New("Manager distribution does not match packageManager integrity")
	}
	return nil
}

func verifyRegistrySignatures(
	manager PackageManager,
	version registryVersion,
	keysRaw []byte,
) error {
	var keySet registryKeys
	if err := decodeUpstreamJSON(keysRaw, &keySet); err != nil {
		return fmt.Errorf("decode registry signing keys: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey, len(keySet.Keys))
	for _, value := range keySet.Keys {
		block, _ := pem.Decode([]byte(value.Key))
		if block == nil {
			continue
		}
		public, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		key, ok := public.(*ecdsa.PublicKey)
		if ok && key.Curve == elliptic.P256() {
			keys[value.KeyID] = key
		}
	}
	message := []byte(string(manager.Name) + "@" + manager.Version + ":" + version.Dist.Integrity)
	digest := sha256.Sum256(message)
	for _, signature := range version.Dist.Signatures {
		key := keys[signature.KeyID]
		raw, err := base64.StdEncoding.Strict().DecodeString(signature.Sig)
		if err == nil && key != nil && ecdsa.VerifyASN1(key, digest[:], raw) {
			return nil
		}
	}
	return errors.New("registry metadata has no valid published signature")
}

func validateRegistryManagerTree(root string, manager PackageManager) error {
	var entrypoint string
	switch manager.Name {
	case PackageManagerNPM:
		entrypoint = "bin/npm-cli.js"
	case PackageManagerPNPM:
		entrypoint = "bin/pnpm.cjs"
	default:
		return errors.New("registry Manager family is invalid")
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(entrypoint)))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Manager distribution entrypoint %q is invalid", entrypoint)
	}
	packageRaw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return err
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(packageRaw, &manifest); err != nil ||
		manifest.Name != string(manager.Name) ||
		manifest.Version != manager.Version {
		return errors.New("Manager distribution package manifest does not match selector")
	}
	return nil
}

type githubRelease struct {
	Assets []struct {
		Digest             string `json:"digest"`
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
	TagName string `json:"tag_name"`
}

func (acquirer PlatformAcquirer) acquireBun(
	ctx context.Context,
	leaseRoot string,
	manager PackageManager,
	policy ManagerPolicy,
) (_ *managerAcquisition, returnErr error) {
	metadata, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		policy.MetadataOrigin+"bun-v"+manager.Version,
		policy.AllowedRedirectHosts,
		maxUpstreamMetadataBytes,
	)
	if err != nil {
		return nil, err
	}
	defer metadata.Close()
	metadataRaw, err := readUpstream(metadata)
	if err != nil {
		return nil, err
	}
	var release githubRelease
	if err := decodeUpstreamJSON(metadataRaw, &release); err != nil ||
		release.TagName != "bun-v"+manager.Version {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			errors.New("Bun release metadata does not match exact selector"),
		)
	}
	const assetName = "bun-linux-x64-baseline.zip"
	origin, _ := ManagerSourceOrigin(manager)
	digest := ""
	for _, asset := range release.Assets {
		if asset.Name != assetName {
			continue
		}
		if digest != "" || asset.BrowserDownloadURL != origin ||
			!strings.HasPrefix(asset.Digest, "sha256:") {
			return nil, deterministicAcquisitionFailure(
				api.WorkerPlatformAcquisitionIntegrityFailed,
				errors.New("Bun release asset metadata is ambiguous or invalid"),
			)
		}
		digest = asset.Digest
	}
	if digest == "" {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			errors.New("Bun release asset has no official digest"),
		)
	}
	distribution, err := fetchUpstream(
		ctx,
		acquirer.HTTP,
		leaseRoot,
		origin,
		policy.AllowedRedirectHosts,
		maxBunDistributionBytes,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if distribution != nil {
			returnErr = errors.Join(returnErr, distribution.Close())
		}
	}()
	if distribution.source.Digest != digest {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionIntegrityFailed,
			errors.New("Bun distribution does not match official release digest"),
		)
	}
	extracted := filepath.Join(leaseRoot, "manager-source")
	if err := extractZip(distribution.file, extracted); err != nil {
		return nil, deterministicAcquisitionFailure(api.WorkerPlatformAcquisitionTopologyFailed, err)
	}
	root := filepath.Join(extracted, "bun-linux-x64-baseline")
	info, err := os.Lstat(filepath.Join(root, "bun"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, deterministicAcquisitionFailure(
			api.WorkerPlatformAcquisitionTopologyFailed,
			errors.New("Bun distribution entrypoint is invalid"),
		)
	}
	result := &managerAcquisition{
		distribution: distribution,
		evidence:     map[string][]byte{"github-release.json": metadataRaw},
		identity:     "github-releases",
		integrity:    "github-sha256",
		root:         root,
	}
	distribution = nil
	return result, nil
}

func extractZip(source *os.File, destination string) error {
	info, err := source.Stat()
	if err != nil {
		return err
	}
	reader, err := zip.NewReader(source, info.Size())
	if err != nil {
		return err
	}
	if len(reader.File) == 0 || len(reader.File) > maxPlatformAcquisitionEntries {
		return errors.New("ZIP entry count is invalid")
	}
	var total int64
	for _, entry := range reader.File {
		name := filepath.ToSlash(entry.Name)
		clean := filepath.Clean(filepath.FromSlash(name))
		if name == "" || strings.HasPrefix(name, "/") || clean == "." ||
			clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("ZIP path %q is unsafe", entry.Name)
		}
		target := filepath.Join(destination, clean)
		mode := entry.Mode()
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() || entry.UncompressedSize64 > uint64(maxArtifactFileSize) ||
			total > maxPlatformAcquisitionTreeBytes-int64(entry.UncompressedSize64) {
			return fmt.Errorf("ZIP entry %q is unsupported or excessive", entry.Name)
		}
		total += int64(entry.UncompressedSize64)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil || closeErr != nil || written != int64(entry.UncompressedSize64) {
			return errors.Join(copyErr, closeErr, errors.New("ZIP entry size mismatch"))
		}
	}
	return nil
}

func decodeUpstreamJSON(raw []byte, destination any) error {
	if _, err := jsoncanon.Transform(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return ensureEOF(decoder, "upstream JSON")
}
