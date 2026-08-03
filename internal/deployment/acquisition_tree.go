package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (acquirer PlatformAcquirer) runtimeTree(
	ctx context.Context,
	leaseRoot string,
	identity string,
	nodeVersion string,
	manager PackageManager,
	policy PlatformAcquisitionPolicy,
	node *nodeAcquisition,
) (_ *platformTree, returnErr error) {
	root := filepath.Join(leaseRoot, "runtime-tree")
	if err := acquirer.extractPlatformInput(ctx, policy.Runtime.Harness, root); err != nil {
		return nil, fmt.Errorf("extract Runtime harness: %w", err)
	}
	if err := copyRegularFile(
		filepath.Join(node.root, "bin", "node"),
		filepath.Join(root, "bin", "node"),
		0755,
	); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	if err := copyRegularFile(
		filepath.Join(node.root, "LICENSE"),
		filepath.Join(root, "share", "licenses", "node", "LICENSE"),
		0644,
	); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	loader := filepath.Join(root, "lib", "ld-linux-x86-64.so.2")
	if info, err := os.Lstat(loader); err != nil || !info.Mode().IsRegular() {
		return nil, deterministicAcquisitionFailure(
			workerapi.PlatformAcquisitionTopologyFailed,
			errors.New("Runtime harness loader is missing"),
		)
	}
	if err := runBoundedCommand(
		ctx,
		acquirer.Patchelf,
		"--set-interpreter", "/opt/helmr/runtime/lib/ld-linux-x86-64.so.2",
		"--set-rpath", "/opt/helmr/runtime/lib",
		filepath.Join(root, "bin", "node"),
	); err != nil {
		return nil, deterministicAcquisitionFailure(
			workerapi.PlatformAcquisitionTopologyFailed,
			fmt.Errorf("patch Runtime Node: %w", err),
		)
	}
	evidence := platformEvidenceSet{
		documents: cloneEvidence(node.evidence),
		source:    node.distribution,
	}
	inputRaw, err := CanonicalPlatformDocument(policy.Runtime)
	if err != nil {
		return nil, err
	}
	evidence.documents["runtime-inputs.json"] = inputRaw
	integrity := PlatformIntegrity{
		Evidence:      platformEvidence(evidence),
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Identity:      node.identity,
		IntegrityKind: "openpgp-sha256",
		Redirects:     append([]string(nil), node.distribution.redirects...),
		Source:        node.distribution.source,
	}
	integrityRaw, err := CanonicalPlatformDocument(integrity)
	if err != nil {
		return nil, err
	}
	descriptor := RuntimeArtifactDescriptor{
		AdapterVersion:          NodeRuntimeAdapterVersion,
		Architecture:            ArchitectureX8664,
		DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
		Entrypoint:              "/opt/helmr/runtime/helmr/entry.mjs",
		IntegrityDigest:         digestDocument(integrityRaw),
		Kind:                    "runtime",
		MediaType:               RuntimeArtifactMediaType,
		NodeModuleABI:           node.moduleABI,
		NodeVersion:             nodeVersion,
		ProgramNodeFlags:        append([]string(nil), policy.NodeFlags...),
		RuntimeAPIVersion:       RuntimeAPIVersion,
		RuntimeHarnessDigest:    policy.Runtime.Harness.Digest,
		Source:                  node.distribution.source,
	}
	pre, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, runtimeArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	conformance, validationErr := acquirer.Validator.Runtime(ctx, identity+"-runtime", pre, descriptor)
	closeErr := pre.Close()
	if validationErr != nil || closeErr != nil {
		return nil, conformanceFailure(validationErr, closeErr)
	}
	if err := normalizeConformance(
		&conformance,
		policy.ConformanceSet,
		platformEvidence(evidence),
	); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionConformanceFailed, err)
	}
	conformanceRaw, err := CanonicalPlatformDocument(conformance)
	if err != nil {
		return nil, err
	}
	descriptor.ConformanceDigest = digestDocument(conformanceRaw)
	if err := writePlatformDocuments(ctx, root, descriptor, integrity, conformance, evidence); err != nil {
		return nil, err
	}
	final, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, runtimeArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	defer func() {
		if final != nil {
			returnErr = errors.Join(returnErr, final.Close())
		}
	}()
	expectations, err := acquirer.Policy.PlatformArtifactExpectations(
		nodeVersion,
		manager,
		"",
	)
	if err != nil {
		return nil, err
	}
	if err := final.validate(ctx, expectations.Runtime); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	result := final
	final = nil
	return result, nil
}

func (acquirer PlatformAcquirer) managerTree(
	ctx context.Context,
	leaseRoot string,
	identity string,
	nodeVersion string,
	manager PackageManager,
	policy PlatformAcquisitionPolicy,
	source *managerAcquisition,
	runtime *platformTree,
) (_ *platformTree, returnErr error) {
	root := filepath.Join(leaseRoot, "manager-tree")
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	kind, entrypoint, _, err := managerDistribution(manager)
	if err != nil {
		return nil, err
	}
	switch manager.Name {
	case PackageManagerBun:
		if err := copyRegularFile(
			filepath.Join(source.root, "bun"),
			filepath.Join(root, "bin", "bun"),
			0755,
		); err != nil {
			return nil, err
		}
	case PackageManagerNPM, PackageManagerPNPM:
		if err := copyDirectory(
			source.root,
			filepath.Join(root, "lib", string(manager.Name)),
		); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("Manager family is unsupported")
	}
	evidence := platformEvidenceSet{
		documents: cloneEvidence(source.evidence),
		source:    source.distribution,
	}
	integrity := PlatformIntegrity{
		Evidence:      platformEvidence(evidence),
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Identity:      source.identity,
		IntegrityKind: source.integrity,
		Redirects:     append([]string(nil), source.distribution.redirects...),
		Source:        source.distribution.source,
	}
	integrityRaw, err := CanonicalPlatformDocument(integrity)
	if err != nil {
		return nil, err
	}
	descriptor := ManagerArtifactDescriptor{
		AdapterVersion:          ManagerAdapterVersion,
		Architecture:            ArchitectureX8664,
		DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
		Entrypoint:              ManagerEntrypoint{Kind: kind, Path: entrypoint},
		IntegrityDigest:         digestDocument(integrityRaw),
		Kind:                    "manager",
		MediaType:               ManagerTreeMediaType,
		PackageManager:          manager,
		Source:                  source.distribution.source,
	}
	pre, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, managerArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	conformance, validationErr := acquirer.Validator.Manager(
		ctx,
		identity+"-manager",
		runtime,
		pre,
		descriptor,
	)
	closeErr := pre.Close()
	if validationErr != nil || closeErr != nil {
		return nil, conformanceFailure(validationErr, closeErr)
	}
	if err := normalizeConformance(
		&conformance,
		policy.ConformanceSet,
		platformEvidence(evidence),
	); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionConformanceFailed, err)
	}
	conformanceRaw, err := CanonicalPlatformDocument(conformance)
	if err != nil {
		return nil, err
	}
	descriptor.ConformanceDigest = digestDocument(conformanceRaw)
	if err := writePlatformDocuments(ctx, root, descriptor, integrity, conformance, evidence); err != nil {
		return nil, err
	}
	final, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, managerArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	defer func() {
		if final != nil {
			returnErr = errors.Join(returnErr, final.Close())
		}
	}()
	runtimeDescriptor, err := runtime.descriptor()
	if err != nil {
		return nil, err
	}
	expectations, err := acquirer.Policy.PlatformArtifactExpectations(
		nodeVersion,
		manager,
		runtimeDescriptor.Digest,
	)
	if err != nil {
		return nil, err
	}
	if err := final.validate(ctx, expectations.Manager); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	result := final
	final = nil
	return result, nil
}

func (acquirer PlatformAcquirer) toolchainTree(
	ctx context.Context,
	leaseRoot string,
	identity string,
	nodeVersion string,
	manager PackageManager,
	policy PlatformAcquisitionPolicy,
	node *nodeAcquisition,
	runtime *platformTree,
	runtimeDigest string,
) (_ *platformTree, returnErr error) {
	root := filepath.Join(leaseRoot, "toolchain-tree")
	if err := acquirer.extractPlatformInput(ctx, policy.Toolchain.Base, root); err != nil {
		return nil, fmt.Errorf("extract toolchain base: %w", err)
	}
	headers := filepath.Join(root, "include", "node")
	if err := copyDirectory(filepath.Join(node.root, "include", "node"), headers); err != nil {
		return nil, err
	}
	headersDigest, err := digestDirectory(ctx, headers)
	if err != nil {
		return nil, err
	}
	inputRaw, err := CanonicalPlatformDocument(struct {
		Base          ArtifactDescriptor `json:"base"`
		Compiler      CompilerInputs     `json:"compiler"`
		RuntimeDigest string             `json:"runtimeDigest"`
	}{
		Base:          policy.Toolchain.Base,
		Compiler:      policy.Toolchain.Compiler,
		RuntimeDigest: runtimeDigest,
	})
	if err != nil {
		return nil, err
	}
	evidence := platformEvidenceSet{
		documents: map[string][]byte{"toolchain-inputs.json": inputRaw},
	}
	source := PlatformSource{
		Digest:    policy.Toolchain.Base.Digest,
		Origin:    "platform-cas:" + policy.Toolchain.Base.Digest,
		SizeBytes: policy.Toolchain.Base.SizeBytes,
	}
	integrity := PlatformIntegrity{
		Evidence:      platformEvidence(evidence),
		FormatVersion: PlatformArtifactDocumentFormatVersion,
		Identity:      "helmr-platform",
		IntegrityKind: "composed-sha256",
		Redirects:     []string{},
		Source:        source,
	}
	integrityRaw, err := CanonicalPlatformDocument(integrity)
	if err != nil {
		return nil, err
	}
	descriptor := ToolchainArtifactDescriptor{
		AdapterVersion:          ToolchainAdapterVersion,
		Architecture:            ArchitectureX8664,
		BaseDigest:              policy.Toolchain.Base.Digest,
		Compiler:                policy.Toolchain.Compiler,
		DescriptorSchemaVersion: policy.DescriptorSchemaVersion,
		IntegrityDigest:         digestDocument(integrityRaw),
		Kind:                    "toolchain",
		MediaType:               ToolchainMediaType,
		NodeHeadersDigest:       headersDigest,
		NodeModuleABI:           node.moduleABI,
		NodeSource:              node.distribution.source,
		NodeVersion:             nodeVersion,
		RuntimeDigest:           runtimeDigest,
	}
	pre, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, toolchainArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	conformance, validationErr := acquirer.Validator.Toolchain(
		ctx,
		identity+"-toolchain",
		runtime,
		pre,
		descriptor,
	)
	closeErr := pre.Close()
	if validationErr != nil || closeErr != nil {
		return nil, conformanceFailure(validationErr, closeErr)
	}
	if err := normalizeConformance(
		&conformance,
		policy.ConformanceSet,
		platformEvidence(evidence),
	); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionConformanceFailed, err)
	}
	conformanceRaw, err := CanonicalPlatformDocument(conformance)
	if err != nil {
		return nil, err
	}
	descriptor.ConformanceDigest = digestDocument(conformanceRaw)
	if err := writePlatformDocuments(ctx, root, descriptor, integrity, conformance, evidence); err != nil {
		return nil, err
	}
	final, err := encodePlatformTree(ctx, acquirer.WorkDir, acquirer.Encoder, toolchainArtifact, root)
	if err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	defer func() {
		if final != nil {
			returnErr = errors.Join(returnErr, final.Close())
		}
	}()
	expectations, err := acquirer.Policy.PlatformArtifactExpectations(
		nodeVersion,
		manager,
		runtimeDigest,
	)
	if err != nil {
		return nil, err
	}
	if err := final.validate(ctx, expectations.Toolchain); err != nil {
		return nil, deterministicAcquisitionFailure(workerapi.PlatformAcquisitionTopologyFailed, err)
	}
	result := final
	final = nil
	return result, nil
}

func (acquirer PlatformAcquirer) extractPlatformInput(
	ctx context.Context,
	descriptor ArtifactDescriptor,
	destination string,
) error {
	object, err := acquirer.Store.Stat(ctx, descriptor.Digest)
	if err != nil {
		return err
	}
	if object.Digest != descriptor.Digest ||
		object.MediaType != descriptor.MediaType ||
		object.SizeBytes != descriptor.SizeBytes {
		return errors.New("Platform input metadata does not match policy")
	}
	body, err := acquirer.Store.Get(ctx, descriptor.Digest)
	if err != nil {
		return err
	}
	defer body.Close()
	hash := sha256.New()
	counting := &limitedReader{
		reader: io.TeeReader(body, hash),
		limit:  descriptor.SizeBytes,
	}
	if _, err := archive.ExtractTarWithStats(counting, destination, archive.ExtractOptions{
		MaxBytes:   maxPlatformAcquisitionTreeBytes,
		MaxEntries: maxPlatformAcquisitionEntries,
	}); err != nil {
		return err
	}
	if counting.read != descriptor.SizeBytes ||
		"sha256:"+hex.EncodeToString(hash.Sum(nil)) != descriptor.Digest {
		return errors.New("Platform input bytes do not match policy")
	}
	return nil
}

type limitedReader struct {
	reader io.Reader
	limit  int64
	read   int64
}

func (reader *limitedReader) Read(buffer []byte) (int, error) {
	if reader.read >= reader.limit {
		var extra [1]byte
		count, err := reader.reader.Read(extra[:])
		if count != 0 {
			return 0, errors.New("Platform input exceeds its declared size")
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.limit-reader.read {
		buffer = buffer[:reader.limit-reader.read]
	}
	count, err := reader.reader.Read(buffer)
	reader.read += int64(count)
	return count, err
}

type platformEvidenceSet struct {
	documents map[string][]byte
	source    *upstreamObject
}

func platformEvidence(evidence platformEvidenceSet) []PlatformEvidenceFile {
	result := make([]PlatformEvidenceFile, 0, len(evidence.documents)+1)
	for name, raw := range evidence.documents {
		result = append(result, PlatformEvidenceFile{
			Digest:    digestDocument(raw),
			Path:      "helmr/upstream/" + name,
			SizeBytes: int64(len(raw)),
		})
	}
	if evidence.source != nil {
		result = append(result, PlatformEvidenceFile{
			Digest:    evidence.source.source.Digest,
			Path:      "helmr/upstream/source",
			SizeBytes: evidence.source.source.SizeBytes,
		})
	}
	return SortedPlatformEvidence(result)
}

func cloneEvidence(source map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(source)+1)
	for name, raw := range source {
		result[name] = bytes.Clone(raw)
	}
	return result
}

func normalizeConformance(
	value *PlatformConformance,
	conformanceSet string,
	inputs []PlatformEvidenceFile,
) error {
	if value == nil {
		return errors.New("Platform conformance result is missing")
	}
	value.FormatVersion = PlatformArtifactDocumentFormatVersion
	value.ConformanceSet = conformanceSet
	value.Inputs = append([]PlatformEvidenceFile(nil), inputs...)
	sort.Slice(value.Results, func(left, right int) bool {
		return value.Results[left].Name < value.Results[right].Name
	})
	raw, err := CanonicalPlatformDocument(*value)
	if err != nil {
		return err
	}
	var parsed PlatformConformance
	if err := parsePlatformDocument(raw, "Platform conformance", &parsed); err != nil {
		return err
	}
	if len(parsed.Results) == 0 {
		return errors.New("Platform conformance result is empty")
	}
	return nil
}

func copyRegularFile(source string, destination string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("source file %q is not regular", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, maxArtifactFileSize+1))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || written != info.Size() ||
		written > maxArtifactFileSize {
		return errors.Join(copyErr, closeErr, errors.New("copied file size changed"))
	}
	return nil
}

func copyDirectory(source string, destination string) error {
	source = filepath.Clean(source)
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) {
		return errors.New("copied tree paths must be absolute")
	}
	return filepath.WalkDir(source, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Mkdir(target, 0755)
		case info.Mode().IsRegular():
			mode := os.FileMode(0644)
			if info.Mode().Perm()&0111 != 0 {
				mode = 0755
			}
			return copyRegularFile(name, target, mode)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if err := validateSymlinkTarget(filepath.ToSlash(link)); err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("source tree path %q has unsupported type", name)
		}
	})
}

func digestDirectory(ctx context.Context, root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%#o\x00", filepath.ToSlash(relative), info.Mode().Type()|info.Mode().Perm()); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(name)
			if err != nil {
				return err
			}
			_, copyErr := copyExact(ctx, hash, file, maxArtifactFileSize)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(hash, target); err != nil {
				return err
			}
		}
		_, err = hash.Write([]byte{0})
		return err
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func runBoundedCommand(ctx context.Context, executable string, arguments ...string) error {
	var stdout, stderr limitedEncoderOutput
	stdout.remaining = maxEncoderDiagnosticBytes
	stderr.remaining = maxEncoderDiagnosticBytes
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = []string{"LC_ALL=C", "TZ=UTC"}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%w%s", err, encoderDiagnostic(&stdout, &stderr))
	}
	return nil
}
