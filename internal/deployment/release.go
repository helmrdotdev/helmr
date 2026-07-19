package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	RuntimeReleaseCatalogFile        = "catalog.json"
	RuntimeReleaseStatementFile      = "attestation.json"
	RuntimeReleaseLineageFile        = "runtime-release.json"
	RuntimeReleaseBundleFile         = "catalog.sigstore.json"
	RuntimeReleaseTrustedRootFile    = "trusted-root.json"
	RuntimeReleaseCorpusFile         = "verifier-corpus.json"
	RuntimeReleaseValidFile          = "verifier-valid.squashfs"
	RuntimeReleaseInvalidFile        = "verifier-invalid.squashfs"
	RuntimeReleaseObjectsDirectory   = "objects"
	ToolchainReleaseDirectory        = "toolchain-release"
	ToolchainReleaseCatalogFile      = ToolchainReleaseDirectory + "/catalog.json"
	ToolchainReleaseStatementFile    = ToolchainReleaseDirectory + "/attestation.json"
	ToolchainReleaseBundleFile       = ToolchainReleaseDirectory + "/catalog.sigstore.json"
	ToolchainReleaseTrustedRootFile  = ToolchainReleaseDirectory + "/trusted-root.json"
	ToolchainReleaseObjectsDirectory = ToolchainReleaseDirectory + "/objects"
	ToolchainSourceFile              = "toolchain.json"
	maxRuntimeReleasePackageBytes    = maxRuntimePhysicalBytes + maxToolchainCorpusBytes + 6*maxProgramFileSizeBytes + 2<<20
	runtimeReleasePackageFileMode    = 0o444
	runtimeReleaseDirectoryFileMode  = 0o444
	runtimeReleaseLineageBytes       = 4096
)

var runtimeReleaseTagPattern = regexp.MustCompile(
	`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))(?:\.(?:(?:0|[1-9][0-9]*)|(?:[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)))*)?$`,
)

var runtimeReleasePackageFiles = []string{
	RuntimeReleaseCatalogFile,
	RuntimeReleaseBundleFile,
	ToolchainReleaseCatalogFile,
	ToolchainReleaseBundleFile,
	ToolchainReleaseTrustedRootFile,
	RuntimeReleaseTrustedRootFile,
	RuntimeReleaseCorpusFile,
	RuntimeReleaseInvalidFile,
	RuntimeReleaseValidFile,
}

type RuntimeReleaseSource struct {
	Runtime                  *os.File
	Invalid                  *os.File
	Descriptor               RuntimeDescriptor
	ScratchDirectory         string
	UnitCgroupRoot           string
	Lineage                  RuntimeReleaseLineage
	Predecessor              *RuntimeReleasePredecessor
	ToolchainSourceDirectory string
}

type RuntimeReleaseLineage struct {
	FormatVersion int                `json:"formatVersion"`
	Release       string             `json:"release"`
	Predecessor   *RuntimeReleaseRef `json:"predecessor"`
}

type RuntimeReleaseRef struct {
	Release   string `json:"release"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type RuntimeReleasePredecessor struct {
	Lineage              RuntimeReleaseLineage
	Catalog              []byte
	Bundle               []byte
	TrustedRoot          []byte
	ToolchainCatalog     []byte
	ToolchainBundle      []byte
	ToolchainTrustedRoot []byte
	Runtimes             map[string]*os.File
	Toolchains           map[string]*os.File
	directory            string
}

type RuntimeRelease struct {
	architecture         RuntimeArchitecture
	lineage              RuntimeReleaseLineage
	catalog              []byte
	statement            []byte
	toolchainCatalog     []byte
	toolchainStatement   []byte
	toolchainBundle      []byte
	toolchainTrustedRoot []byte
	corpus               []byte
	invalid              RuntimeDescriptor
	index                RuntimeIndex
	valid                RuntimeDescriptor
	objects              map[string]*RuntimeArtifactSnapshot
	toolchainObjects     map[string]*toolchainSnapshot
	invalidFile          *RuntimeArtifactSnapshot
	closed               bool
}

type RuntimeReleasePackage struct {
	Digest    string
	SizeBytes int64
}

type runtimeReleaseVerifier func(
	context.Context,
	string,
	string,
	*RuntimeArtifactSnapshot,
) (RuntimeIndex, error)

type runtimeCatalogAuthenticator func([]byte, []byte, []byte) (*RuntimeCatalog, error)
type toolchainCatalogAuthenticator func([]byte, []byte, []byte) (*ToolchainCatalog, error)

type runtimeReleaseLineageAuthenticator func(
	RuntimeReleaseLineage,
	[]byte,
	[]byte,
	[]byte,
) (*RuntimeCatalog, error)
type toolchainReleaseLineageAuthenticator func(
	RuntimeReleaseLineage,
	[]byte,
	[]byte,
	[]byte,
) (*ToolchainCatalog, error)

type runtimeReleaseSnapshotter func(
	context.Context,
	string,
	RuntimeDescriptor,
	*os.File,
) (*RuntimeArtifactSnapshot, error)

type toolchainReleaseSnapshotter func(
	context.Context,
	string,
	ManagerArtifact,
	io.Reader,
) (*toolchainSnapshot, error)

type runtimeReleaseCountingReader struct {
	source io.Reader
	count  int64
}

func ParseRuntimeReleaseLineage(raw []byte, release string) (RuntimeReleaseLineage, error) {
	if len(raw) == 0 || len(raw) > runtimeReleaseLineageBytes {
		return RuntimeReleaseLineage{}, errors.New(
			"runtime release lineage size is outside its v0 bound",
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return RuntimeReleaseLineage{}, fmt.Errorf(
			"canonicalize runtime release lineage: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return RuntimeReleaseLineage{}, errors.New(
			"runtime release lineage is not RFC 8785 canonical JSON",
		)
	}
	var lineage RuntimeReleaseLineage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lineage); err != nil {
		return RuntimeReleaseLineage{}, fmt.Errorf(
			"decode runtime release lineage: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "runtime release lineage"); err != nil {
		return RuntimeReleaseLineage{}, err
	}
	if err := ValidateRuntimeReleaseLineage(lineage, release); err != nil {
		return RuntimeReleaseLineage{}, err
	}
	complete, err := CanonicalRuntimeReleaseLineage(lineage)
	if err != nil {
		return RuntimeReleaseLineage{}, err
	}
	if !bytes.Equal(raw, complete) {
		return RuntimeReleaseLineage{}, errors.New(
			"runtime release lineage does not match the complete closed v0 shape",
		)
	}
	return lineage, nil
}

func CanonicalRuntimeReleaseLineage(
	lineage RuntimeReleaseLineage,
) ([]byte, error) {
	if err := ValidateRuntimeReleaseLineage(lineage, lineage.Release); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(lineage)
	if err != nil {
		return nil, fmt.Errorf("encode runtime release lineage: %w", err)
	}
	return jsoncanon.Transform(raw)
}

func ValidateRuntimeReleaseLineage(
	lineage RuntimeReleaseLineage,
	release string,
) error {
	if lineage.FormatVersion != 0 {
		return fmt.Errorf(
			"runtime release lineage formatVersion = %d, want 0",
			lineage.FormatVersion,
		)
	}
	if !runtimeReleaseTagPattern.MatchString(release) ||
		lineage.Release != release {
		return fmt.Errorf(
			"runtime release lineage release = %q, want exact tag %q",
			lineage.Release,
			release,
		)
	}
	if lineage.Predecessor != nil {
		if err := ValidateRuntimeReleaseRef(*lineage.Predecessor); err != nil {
			return fmt.Errorf("runtime release predecessor: %w", err)
		}
		if lineage.Predecessor.Release == lineage.Release {
			return errors.New("runtime release predecessor equals the successor")
		}
	}
	return nil
}

func ValidateRuntimeReleaseRef(reference RuntimeReleaseRef) error {
	if !runtimeReleaseTagPattern.MatchString(reference.Release) {
		return fmt.Errorf("release %q is not a platform release tag", reference.Release)
	}
	if _, err := RuntimeDigestBytes(reference.Digest); err != nil {
		return err
	}
	if reference.SizeBytes < 1 || reference.SizeBytes > maxJSONSafeInteger {
		return errors.New("sizeBytes is not a positive JavaScript-safe integer")
	}
	return nil
}

func (reader *runtimeReleaseCountingReader) Read(destination []byte) (int, error) {
	count, err := reader.source.Read(destination)
	reader.count += int64(count)
	return count, err
}

func openRuntimeReleaseDirectory(
	directory,
	release string,
) (*RuntimeReleasePredecessor, error) {
	return openRuntimeReleaseDirectoryWithAuthenticators(
		directory,
		release,
		func(
			lineage RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*RuntimeCatalog, error) {
			return VerifyRuntimeCatalogForRelease(
				catalog,
				bundle,
				trustedRoot,
				lineage.Release,
				lineage.Predecessor,
			)
		},
		func(
			lineage RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*ToolchainCatalog, error) {
			return VerifyToolchainCatalogForRelease(
				catalog,
				bundle,
				trustedRoot,
				lineage.Release,
				lineage.Predecessor,
			)
		},
	)
}

func openRuntimeReleaseDirectoryWithAuthenticators(
	directory,
	release string,
	authenticate runtimeReleaseLineageAuthenticator,
	authenticateToolchain func(
		RuntimeReleaseLineage,
		[]byte,
		[]byte,
		[]byte,
	) (*ToolchainCatalog, error),
) (*RuntimeReleasePredecessor, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New(
			"runtime release predecessor directory is not canonical absolute",
		)
	}
	if authenticate == nil || authenticateToolchain == nil {
		return nil, errors.New(
			"runtime release predecessor authenticator is nil",
		)
	}
	catalogBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseCatalogFile),
		maxRuntimeCatalogBytes,
	)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseBundleFile),
		maxReleaseBundleBytes,
	)
	if err != nil {
		return nil, err
	}
	trustedRootBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseTrustedRootFile),
		maxReleaseTrustedRootBytes,
	)
	if err != nil {
		return nil, err
	}
	lineageBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseLineageFile),
		runtimeReleaseLineageBytes,
	)
	if err != nil {
		return nil, err
	}
	lineage, err := ParseRuntimeReleaseLineage(lineageBytes, release)
	if err != nil {
		return nil, err
	}
	catalog, err := authenticate(
		lineage,
		catalogBytes,
		bundleBytes,
		trustedRootBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("authenticate predecessor runtime release: %w", err)
	}
	statementBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseStatementFile),
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return nil, err
	}
	statement, err := canonicalRuntimeReleaseStatement(
		catalogBytes,
		catalog,
		lineage.Predecessor,
	)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(statementBytes, statement) {
		return nil, errors.New(
			"runtime release statement does not exact-match its signed payload",
		)
	}
	toolchainCatalogBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseCatalogFile),
		maxToolchainCatalogBytes,
	)
	if err != nil {
		return nil, err
	}
	toolchainBundleBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseBundleFile),
		maxReleaseBundleBytes,
	)
	if err != nil {
		return nil, err
	}
	toolchainTrustedRootBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseTrustedRootFile),
		maxReleaseTrustedRootBytes,
	)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(trustedRootBytes, toolchainTrustedRootBytes) {
		return nil, errors.New(
			"runtime and standard-toolchain trusted roots do not exact-match",
		)
	}
	toolchainCatalog, err := authenticateToolchain(
		lineage,
		toolchainCatalogBytes,
		toolchainBundleBytes,
		toolchainTrustedRootBytes,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"authenticate predecessor standard-toolchain release: %w",
			err,
		)
	}
	toolchainStatementBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseStatementFile),
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return nil, err
	}
	toolchainStatement, err := canonicalToolchainReleaseStatement(
		toolchainCatalogBytes,
		toolchainCatalog,
		lineage.Predecessor,
	)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(toolchainStatementBytes, toolchainStatement) {
		return nil, errors.New(
			"standard-toolchain release statement does not exact-match its signed payload",
		)
	}
	predecessor := &RuntimeReleasePredecessor{
		Lineage:              lineage,
		Catalog:              catalogBytes,
		Bundle:               bundleBytes,
		TrustedRoot:          trustedRootBytes,
		ToolchainCatalog:     toolchainCatalogBytes,
		ToolchainBundle:      toolchainBundleBytes,
		ToolchainTrustedRoot: toolchainTrustedRootBytes,
		Runtimes:             make(map[string]*os.File, len(catalog.runtimes)),
		Toolchains:           make(map[string]*os.File),
	}
	for _, descriptor := range catalog.runtimes {
		path := filepath.Join(
			directory,
			RuntimeReleaseObjectsDirectory,
			"sha256",
			strings.TrimPrefix(descriptor.Digest, "sha256:"),
		)
		file, err := OpenRuntimeReleaseFile(path, descriptor.SizeBytes)
		if err != nil {
			predecessor.Close()
			return nil, fmt.Errorf(
				"open predecessor runtime %q: %w",
				descriptor.Digest,
				err,
			)
		}
		stat, err := file.Stat()
		if err != nil || stat.Size() != descriptor.SizeBytes {
			file.Close()
			predecessor.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf(
				"predecessor runtime %q has size %d, want %d",
				descriptor.Digest,
				stat.Size(),
				descriptor.SizeBytes,
			)
		}
		predecessor.Runtimes[descriptor.Digest] = file
	}
	if err := validateRuntimeReleaseObjectDirectory(directory, catalog); err != nil {
		predecessor.Close()
		return nil, err
	}
	toolchainObjects, err := toolchainClosureObjects(
		toolchainCatalog,
		"",
	)
	if err != nil {
		predecessor.Close()
		return nil, err
	}
	for _, descriptor := range toolchainObjects {
		path := filepath.Join(
			directory,
			ToolchainReleaseObjectsDirectory,
			"sha256",
			strings.TrimPrefix(descriptor.Digest, "sha256:"),
		)
		file, openErr := OpenRuntimeReleaseFile(path, descriptor.SizeBytes)
		if openErr != nil {
			predecessor.Close()
			return nil, fmt.Errorf(
				"open predecessor standard toolchain %q: %w",
				descriptor.Digest,
				openErr,
			)
		}
		predecessor.Toolchains[descriptor.Digest] = file
	}
	if err := validateToolchainObjectDirectory(
		directory,
		toolchainObjects,
	); err != nil {
		predecessor.Close()
		return nil, err
	}
	return predecessor, nil
}

func (predecessor *RuntimeReleasePredecessor) Close() error {
	if predecessor == nil {
		return nil
	}
	var closeErr error
	for digest, file := range predecessor.Runtimes {
		if file != nil {
			if err := file.Close(); err != nil {
				closeErr = errors.Join(
					closeErr,
					fmt.Errorf(
						"close predecessor runtime %q: %w",
						digest,
						err,
					),
				)
			}
		}
		delete(predecessor.Runtimes, digest)
	}
	for digest, file := range predecessor.Toolchains {
		if file != nil {
			if err := file.Close(); err != nil {
				closeErr = errors.Join(
					closeErr,
					fmt.Errorf(
						"close predecessor standard toolchain %q: %w",
						digest,
						err,
					),
				)
			}
		}
		delete(predecessor.Toolchains, digest)
	}
	if predecessor.directory != "" {
		closeErr = errors.Join(closeErr, os.RemoveAll(predecessor.directory))
		predecessor.directory = ""
	}
	return closeErr
}

func validateRuntimeReleaseObjectDirectory(
	directory string,
	catalog *RuntimeCatalog,
) error {
	root := filepath.Join(directory, RuntimeReleaseObjectsDirectory, "sha256")
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read predecessor runtime object directory: %w", err)
	}
	expected := make(map[string]bool, len(catalog.runtimes))
	for _, descriptor := range catalog.runtimes {
		expected[strings.TrimPrefix(descriptor.Digest, "sha256:")] = true
	}
	if len(entries) != len(expected) {
		return fmt.Errorf(
			"predecessor runtime object count = %d, want %d",
			len(entries),
			len(expected),
		)
	}
	for _, entry := range entries {
		if entry.IsDir() || !expected[entry.Name()] {
			return fmt.Errorf(
				"predecessor contains unexpected runtime object %q",
				entry.Name(),
			)
		}
	}
	return nil
}

func validateToolchainObjectDirectory(
	directory string,
	objects []ToolObject,
) error {
	root := filepath.Join(
		directory,
		ToolchainReleaseObjectsDirectory,
		"sha256",
	)
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read standard-toolchain object directory: %w", err)
	}
	expected := make(map[string]bool, len(objects))
	for _, descriptor := range objects {
		expected[strings.TrimPrefix(descriptor.Digest, "sha256:")] = true
	}
	if len(entries) != len(expected) {
		return fmt.Errorf(
			"standard-toolchain object count = %d, want %d",
			len(entries),
			len(expected),
		)
	}
	for _, entry := range entries {
		if entry.IsDir() || !expected[entry.Name()] {
			return fmt.Errorf(
				"standard-toolchain release contains unexpected object %q",
				entry.Name(),
			)
		}
	}
	return nil
}

func PrepareRuntimeRelease(
	ctx context.Context,
	source RuntimeReleaseSource,
) (*RuntimeRelease, error) {
	var predecessor *RuntimeCatalog
	var predecessorToolchains *ToolchainCatalog
	if source.Predecessor != nil {
		if source.Lineage.Predecessor == nil {
			return nil, errors.New(
				"runtime release predecessor is absent from lineage",
			)
		}
		var err error
		predecessor, err = VerifyRuntimeCatalogForRelease(
			source.Predecessor.Catalog,
			source.Predecessor.Bundle,
			source.Predecessor.TrustedRoot,
			source.Lineage.Predecessor.Release,
			source.Predecessor.Lineage.Predecessor,
		)
		if err != nil {
			return nil, fmt.Errorf("authenticate predecessor runtime catalog: %w", err)
		}
		if !bytes.Equal(
			source.Predecessor.TrustedRoot,
			source.Predecessor.ToolchainTrustedRoot,
		) {
			return nil, errors.New(
				"predecessor runtime and standard-toolchain trusted roots do not exact-match",
			)
		}
		predecessorToolchains, err = VerifyToolchainCatalogForRelease(
			source.Predecessor.ToolchainCatalog,
			source.Predecessor.ToolchainBundle,
			source.Predecessor.ToolchainTrustedRoot,
			source.Lineage.Predecessor.Release,
			source.Predecessor.Lineage.Predecessor,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"authenticate predecessor standard-toolchain catalog: %w",
				err,
			)
		}
	}
	return prepareRuntimeRelease(
		ctx,
		source,
		predecessor,
		predecessorToolchains,
		VerifyRuntimeArtifact,
		snapshotRuntimeReleaseFile,
		snapshotToolchain,
	)
}

func prepareRuntimeRelease(
	ctx context.Context,
	source RuntimeReleaseSource,
	predecessor *RuntimeCatalog,
	predecessorToolchains *ToolchainCatalog,
	verify runtimeReleaseVerifier,
	snapshot runtimeReleaseSnapshotter,
	snapshotToolchainClosure toolchainReleaseSnapshotter,
) (_ *RuntimeRelease, returnErr error) {
	if ctx == nil {
		return nil, errors.New("runtime release context is nil")
	}
	if source.Runtime == nil || source.Invalid == nil {
		return nil, errors.New("runtime release source fixtures are incomplete")
	}
	if source.ScratchDirectory == "" {
		return nil, errors.New("runtime release scratch directory is empty")
	}
	if source.UnitCgroupRoot == "" {
		return nil, errors.New("runtime release verifier cgroup root is empty")
	}
	if verify == nil || snapshot == nil || snapshotToolchainClosure == nil {
		return nil, errors.New("runtime release operations are nil")
	}
	if err := ValidateRuntimeDescriptor(source.Descriptor); err != nil {
		return nil, err
	}
	if source.Descriptor.SizeBytes > maxRuntimePhysicalBytes {
		return nil, fmt.Errorf("runtime release source exceeds %d bytes", maxRuntimePhysicalBytes)
	}
	if err := ValidateRuntimeReleaseLineage(
		source.Lineage,
		source.Lineage.Release,
	); err != nil {
		return nil, err
	}
	reopen := source.Predecessor != nil &&
		source.Predecessor.Lineage.Release == source.Lineage.Release
	if !reopen &&
		(source.Lineage.Predecessor == nil) != (source.Predecessor == nil) {
		return nil, errors.New(
			"runtime release lineage and predecessor distribution disagree",
		)
	}
	if (source.Predecessor == nil) != (predecessor == nil) ||
		(source.Predecessor == nil) != (predecessorToolchains == nil) {
		return nil, errors.New("runtime release predecessor authentication is incomplete")
	}

	release := &RuntimeRelease{
		architecture:     source.Descriptor.Architecture,
		lineage:          source.Lineage,
		valid:            source.Descriptor,
		objects:          make(map[string]*RuntimeArtifactSnapshot),
		toolchainObjects: make(map[string]*toolchainSnapshot),
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, release.Close())
		}
	}()

	if predecessor != nil {
		if err := capturePredecessorRuntimeObjects(
			ctx,
			source.ScratchDirectory,
			source.Predecessor,
			predecessor,
			release.objects,
			snapshot,
		); err != nil {
			return nil, err
		}
	}

	valid, err := snapshot(
		ctx,
		source.ScratchDirectory,
		source.Descriptor,
		source.Runtime,
	)
	if err != nil {
		return nil, fmt.Errorf("capture managed runtime: %w", err)
	}
	if inherited := release.objects[source.Descriptor.Digest]; inherited != nil {
		if err := inherited.Close(); err != nil {
			return nil, fmt.Errorf("close duplicate predecessor runtime: %w", err)
		}
	}
	release.objects[source.Descriptor.Digest] = valid

	index, err := verify(
		ctx,
		source.UnitCgroupRoot,
		"release-valid",
		valid,
	)
	if err != nil {
		return nil, fmt.Errorf("verify managed runtime release: %w", err)
	}
	if index.Architecture != source.Descriptor.Architecture ||
		index.RuntimeAPIVersion != source.Descriptor.RuntimeAPIVersion {
		return nil, errors.New("verified managed runtime index does not match its descriptor")
	}
	release.index = index

	invalidDescriptor, err := runtimeReleaseFileDescriptor(
		source.Descriptor.Architecture,
		source.Invalid,
	)
	if err != nil {
		return nil, fmt.Errorf("describe invalid runtime verifier fixture: %w", err)
	}
	invalid, err := snapshot(
		ctx,
		source.ScratchDirectory,
		invalidDescriptor,
		source.Invalid,
	)
	if err != nil {
		return nil, fmt.Errorf("capture invalid runtime verifier fixture: %w", err)
	}
	release.invalid = invalidDescriptor
	release.invalidFile = invalid
	_, verifyErr := verify(
		ctx,
		source.UnitCgroupRoot,
		"release-invalid",
		invalid,
	)
	var semanticInvalid *verifierInvalidError
	if errors.As(verifyErr, &semanticInvalid) {
		verifyErr = nil
	} else {
		if verifyErr == nil {
			verifyErr = errors.New("runtime verifier accepted the invalid release fixture")
		} else {
			verifyErr = fmt.Errorf(
				"invalid release fixture did not produce semantic invalidity: %w",
				verifyErr,
			)
		}
	}
	if err := verifyErr; err != nil {
		return nil, err
	}

	runtimes := make([]RuntimeDescriptor, 0, len(release.objects))
	for _, snapshot := range release.objects {
		runtimes = append(runtimes, snapshot.descriptor)
	}
	sort.Slice(runtimes, func(first, second int) bool {
		return runtimes[first].Digest < runtimes[second].Digest
	})
	if err := validateRuntimeCatalogSuccessor(predecessor, runtimes); err != nil {
		return nil, err
	}
	release.catalog, err = CanonicalRuntimeCatalog(runtimes)
	if err != nil {
		return nil, err
	}
	catalog, err := ParseRuntimeCatalog(release.catalog)
	if err != nil {
		return nil, err
	}
	release.statement, err = canonicalRuntimeReleaseStatement(
		release.catalog,
		catalog,
		release.lineage.Predecessor,
	)
	if err != nil {
		return nil, err
	}
	var toolchains []Toolchain
	if predecessorToolchains != nil {
		toolchains = append(toolchains, predecessorToolchains.toolchains...)
		if err := capturePredecessorToolchainObjects(
			ctx,
			source.ScratchDirectory,
			source.Predecessor,
			predecessorToolchains,
			release.toolchainObjects,
			snapshotToolchainClosure,
		); err != nil {
			return nil, err
		}
	}
	if source.ToolchainSourceDirectory != "" {
		toolchain, file, openErr := openToolchainSource(
			source.ToolchainSourceDirectory,
		)
		if openErr != nil {
			return nil, openErr
		}
		captured, captureErr := snapshotToolchainClosure(
			ctx,
			source.ScratchDirectory,
			toolchain.ToolchainClosure,
			file,
		)
		closeErr := file.Close()
		if captureErr != nil || closeErr != nil {
			return nil, errors.Join(captureErr, closeErr)
		}
		if toolchain.Architecture != source.Descriptor.Architecture ||
			toolchain.ManagedRuntimeDigest != source.Descriptor.Digest {
			captured.Close()
			return nil, errors.New(
				"standard-toolchain source does not target the composed managed runtime",
			)
		}
		digest, digestErr := StandardToolchainDigest(toolchain)
		if digestErr != nil {
			captured.Close()
			return nil, digestErr
		}
		found := false
		for _, existing := range toolchains {
			existingDigest, digestErr := StandardToolchainDigest(existing)
			if digestErr != nil {
				captured.Close()
				return nil, digestErr
			}
			if existingDigest == digest {
				found = true
				if existing != toolchain {
					captured.Close()
					return nil, fmt.Errorf(
						"standard-toolchain catalog successor mutates predecessor digest %q",
						digest,
					)
				}
				break
			}
		}
		if !found {
			toolchains = append(toolchains, toolchain)
		}
		closureDigest := toolchain.ToolchainClosure.Digest
		if inherited := release.toolchainObjects[closureDigest]; inherited != nil {
			if inherited.descriptor != captured.descriptor {
				captured.Close()
				return nil, fmt.Errorf(
					"standard-toolchain closure digest %q has divergent descriptors",
					closureDigest,
				)
			}
			if err := captured.Close(); err != nil {
				return nil, err
			}
		} else {
			release.toolchainObjects[closureDigest] = captured
		}
	} else if predecessorToolchains == nil {
		return nil, errors.New("runtime release standard-toolchain source is empty")
	}
	sort.Slice(toolchains, func(left, right int) bool {
		leftDigest, _ := StandardToolchainDigest(toolchains[left])
		rightDigest, _ := StandardToolchainDigest(toolchains[right])
		return leftDigest < rightDigest
	})
	release.toolchainCatalog, err = CanonicalToolchainCatalog(toolchains)
	if err != nil {
		return nil, err
	}
	toolchainCatalog, err := ParseToolchainCatalog(release.toolchainCatalog)
	if err != nil {
		return nil, err
	}
	expectedObjects, err := toolchainClosureObjects(toolchainCatalog, "")
	if err != nil {
		return nil, err
	}
	if err := validateCapturedToolchainObjects(
		release.toolchainObjects,
		expectedObjects,
	); err != nil {
		return nil, err
	}
	release.toolchainStatement, err = canonicalToolchainReleaseStatement(
		release.toolchainCatalog,
		toolchainCatalog,
		release.lineage.Predecessor,
	)
	if err != nil {
		return nil, err
	}
	release.corpus, err = canonicalRuntimeVerifierCorpusManifest(
		runtimeVerifierCorpusManifest{
			FormatVersion: RuntimeVerifierCorpusFormatVersion,
			Valid: runtimeVerifierCorpusValid{
				Descriptor:    source.Descriptor,
				ExpectedIndex: index,
			},
			Invalid: runtimeVerifierCorpusInvalid{
				Descriptor: release.invalid,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return release, nil
}

func capturePredecessorRuntimeObjects(
	ctx context.Context,
	scratchDirectory string,
	input *RuntimeReleasePredecessor,
	catalog *RuntimeCatalog,
	destination map[string]*RuntimeArtifactSnapshot,
	snapshot runtimeReleaseSnapshotter,
) error {
	if input == nil || catalog == nil || !catalog.authenticated {
		return errors.New("authenticated predecessor runtime release is required")
	}
	if len(input.Runtimes) != len(catalog.runtimes) {
		return fmt.Errorf(
			"predecessor runtime object count = %d, want %d",
			len(input.Runtimes),
			len(catalog.runtimes),
		)
	}
	for _, descriptor := range catalog.runtimes {
		source := input.Runtimes[descriptor.Digest]
		if source == nil {
			return fmt.Errorf(
				"predecessor runtime object %q is missing",
				descriptor.Digest,
			)
		}
		captured, err := snapshot(
			ctx,
			scratchDirectory,
			descriptor,
			source,
		)
		if err != nil {
			return fmt.Errorf(
				"capture predecessor runtime %q: %w",
				descriptor.Digest,
				err,
			)
		}
		destination[descriptor.Digest] = captured
	}
	for digest := range input.Runtimes {
		if _, err := catalog.Resolve(digest); err != nil {
			return fmt.Errorf("predecessor contains unexpected runtime object %q", digest)
		}
	}
	return nil
}

func openToolchainSource(
	directory string,
) (Toolchain, *os.File, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return Toolchain{}, nil, errors.New(
			"standard-toolchain source directory is not canonical absolute",
		)
	}
	if err := requireReleaseDirectoryEntries(
		directory,
		[]string{ToolchainSourceFile, "objects"},
		"standard-toolchain source",
	); err != nil {
		return Toolchain{}, nil, err
	}
	raw, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainSourceFile),
		maxToolchainBytes,
	)
	if err != nil {
		return Toolchain{}, nil, err
	}
	toolchain, err := ParseToolchain(raw)
	if err != nil {
		return Toolchain{}, nil, err
	}
	objectRoot := filepath.Join(directory, "objects")
	if err := requireReleaseDirectoryEntries(
		objectRoot,
		[]string{"sha256"},
		"standard-toolchain source objects",
	); err != nil {
		return Toolchain{}, nil, err
	}
	digestRoot := filepath.Join(objectRoot, "sha256")
	name := strings.TrimPrefix(
		toolchain.ToolchainClosure.Digest,
		"sha256:",
	)
	if err := requireReleaseDirectoryEntries(
		digestRoot,
		[]string{name},
		"standard-toolchain source sha256 objects",
	); err != nil {
		return Toolchain{}, nil, err
	}
	file, err := OpenRuntimeReleaseFile(
		filepath.Join(digestRoot, name),
		toolchain.ToolchainClosure.SizeBytes,
	)
	if err != nil {
		return Toolchain{}, nil, fmt.Errorf(
			"open standard-toolchain source closure: %w",
			err,
		)
	}
	return toolchain, file, nil
}

func requireReleaseDirectoryEntries(
	directory string,
	expected []string,
	label string,
) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read %s directory: %w", label, err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	sort.Strings(names)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if strings.Join(names, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf(
			"%s entries = %q, want %q",
			label,
			names,
			want,
		)
	}
	return nil
}

func capturePredecessorToolchainObjects(
	ctx context.Context,
	scratchDirectory string,
	input *RuntimeReleasePredecessor,
	catalog *ToolchainCatalog,
	destination map[string]*toolchainSnapshot,
	snapshot toolchainReleaseSnapshotter,
) error {
	if input == nil || catalog == nil || !catalog.authenticated {
		return errors.New(
			"authenticated predecessor standard-toolchain release is required",
		)
	}
	objects, err := toolchainClosureObjects(catalog, "")
	if err != nil {
		return err
	}
	if len(input.Toolchains) != len(objects) {
		return fmt.Errorf(
			"predecessor standard-toolchain object count = %d, want %d",
			len(input.Toolchains),
			len(objects),
		)
	}
	for _, descriptor := range objects {
		source := input.Toolchains[descriptor.Digest]
		if source == nil {
			return fmt.Errorf(
				"predecessor standard-toolchain object %q is missing",
				descriptor.Digest,
			)
		}
		captured, err := snapshot(
			ctx,
			scratchDirectory,
			ManagerArtifact(descriptor),
			source,
		)
		if err != nil {
			return fmt.Errorf(
				"capture predecessor standard toolchain %q: %w",
				descriptor.Digest,
				err,
			)
		}
		destination[descriptor.Digest] = captured
	}
	return validateCapturedToolchainObjects(destination, objects)
}

func validateCapturedToolchainObjects(
	captured map[string]*toolchainSnapshot,
	expected []ToolObject,
) error {
	if len(captured) != len(expected) {
		return fmt.Errorf(
			"captured standard-toolchain object count = %d, want %d",
			len(captured),
			len(expected),
		)
	}
	for _, descriptor := range expected {
		snapshot := captured[descriptor.Digest]
		if snapshot == nil || snapshot.content == nil {
			return fmt.Errorf(
				"captured standard-toolchain object %q is missing",
				descriptor.Digest,
			)
		}
		if snapshot.descriptor != ManagerArtifact(descriptor) {
			return fmt.Errorf(
				"captured standard-toolchain object %q descriptor drifted",
				descriptor.Digest,
			)
		}
	}
	for digest := range captured {
		found := false
		for _, descriptor := range expected {
			found = found || descriptor.Digest == digest
		}
		if !found {
			return fmt.Errorf(
				"captured standard-toolchain object %q is unexpected",
				digest,
			)
		}
	}
	return nil
}

func snapshotRuntimeReleaseFile(
	ctx context.Context,
	scratchDirectory string,
	descriptor RuntimeDescriptor,
	source *os.File,
) (*RuntimeArtifactSnapshot, error) {
	if source == nil {
		return nil, errors.New("runtime release file is nil")
	}
	stat, err := source.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat runtime release file: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("runtime release file is not regular")
	}
	if stat.Size() != descriptor.SizeBytes {
		return nil, fmt.Errorf(
			"runtime release file size = %d, want %d",
			stat.Size(),
			descriptor.SizeBytes,
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind runtime release file: %w", err)
	}
	return SnapshotRuntimeArtifact(ctx, scratchDirectory, descriptor, source)
}

func validateRuntimeCatalogSuccessor(
	predecessor *RuntimeCatalog,
	successor []RuntimeDescriptor,
) error {
	if predecessor == nil {
		return nil
	}
	if !predecessor.authenticated {
		return errors.New("runtime catalog predecessor is not authenticated")
	}
	byDigest := make(map[string]RuntimeDescriptor, len(successor))
	for _, descriptor := range successor {
		if prior, exists := byDigest[descriptor.Digest]; exists && prior != descriptor {
			return fmt.Errorf("runtime catalog successor mutates digest %q", descriptor.Digest)
		}
		byDigest[descriptor.Digest] = descriptor
	}
	for _, descriptor := range predecessor.runtimes {
		next, exists := byDigest[descriptor.Digest]
		if !exists {
			return fmt.Errorf(
				"runtime catalog successor removes predecessor digest %q",
				descriptor.Digest,
			)
		}
		if next != descriptor {
			return fmt.Errorf(
				"runtime catalog successor mutates predecessor digest %q",
				descriptor.Digest,
			)
		}
	}
	return nil
}

func canonicalRuntimeReleaseStatement(
	catalogBytes []byte,
	catalog *RuntimeCatalog,
	predecessor *RuntimeReleaseRef,
) ([]byte, error) {
	if catalog == nil {
		return nil, errors.New("runtime release catalog is nil")
	}
	catalogHash := sha256.Sum256(catalogBytes)
	subjects := make([]releaseAttestationSubject, 0, len(catalog.runtimes)+1)
	subjects = append(subjects, releaseAttestationSubject{
		Name:   "catalog",
		Digest: map[string]string{"sha256": hex.EncodeToString(catalogHash[:])},
	})
	for _, descriptor := range catalog.runtimes {
		hexDigest := strings.TrimPrefix(descriptor.Digest, "sha256:")
		subjects = append(subjects, releaseAttestationSubject{
			Name:   "runtime/sha256/" + hexDigest,
			Digest: map[string]string{"sha256": hexDigest},
		})
	}
	return canonicalRuntimeAttestationDocument(runtimeAttestationDocument{
		Type:          RuntimeAttestationType,
		Subject:       subjects,
		PredicateType: RuntimeAttestationPredicateType,
		Predicate: runtimeAttestationPredicate{
			CatalogDigest:    "sha256:" + hex.EncodeToString(catalogHash[:]),
			CatalogMediaType: RuntimeCatalogMediaType,
			FormatVersion:    RuntimeAttestationFormatVersion,
			Predecessor:      predecessor,
			Runtimes:         append([]RuntimeDescriptor(nil), catalog.runtimes...),
		},
	})
}

func canonicalToolchainReleaseStatement(
	catalogBytes []byte,
	catalog *ToolchainCatalog,
	predecessor *RuntimeReleaseRef,
) ([]byte, error) {
	if catalog == nil {
		return nil, errors.New("standard-toolchain release catalog is nil")
	}
	catalogHash := sha256.Sum256(catalogBytes)
	subjects := make([]releaseAttestationSubject, 0, len(catalog.toolchains)+1)
	subjects = append(subjects, releaseAttestationSubject{
		Name:   "catalog",
		Digest: map[string]string{"sha256": hex.EncodeToString(catalogHash[:])},
	})
	seenClosures := make(map[string]struct{}, len(catalog.toolchains))
	for _, toolchain := range catalog.toolchains {
		hexDigest := strings.TrimPrefix(
			toolchain.ToolchainClosure.Digest,
			"sha256:",
		)
		if _, exists := seenClosures[hexDigest]; exists {
			continue
		}
		seenClosures[hexDigest] = struct{}{}
		subjects = append(subjects, releaseAttestationSubject{
			Name:   "toolchain/sha256/" + hexDigest,
			Digest: map[string]string{"sha256": hexDigest},
		})
	}
	return canonicalToolchainAttestationDocument(toolchainAttestationDocument{
		Type:          RuntimeAttestationType,
		Subject:       subjects,
		PredicateType: ToolchainAttestationPredicateType,
		Predicate: toolchainAttestationPredicate{
			CatalogDigest: "sha256:" +
				hex.EncodeToString(catalogHash[:]),
			CatalogMediaType: ToolchainCatalogMediaType,
			FormatVersion:    ToolchainCatalogFormatVersion,
			Predecessor:      predecessor,
			Toolchains: append(
				[]Toolchain(nil),
				catalog.toolchains...,
			),
		},
	})
}

func runtimeReleaseDescriptor(
	architecture RuntimeArchitecture,
	raw []byte,
) RuntimeDescriptor {
	sum := sha256.Sum256(raw)
	return RuntimeDescriptor{
		Architecture:      architecture,
		Digest:            "sha256:" + hex.EncodeToString(sum[:]),
		FormatVersion:     RuntimeDescriptorFormatVersion,
		MediaType:         RuntimeArtifactMediaType,
		RuntimeAPIVersion: RuntimeAPIVersion,
		SizeBytes:         int64(len(raw)),
	}
}

func runtimeReleaseFileDescriptor(
	architecture RuntimeArchitecture,
	source *os.File,
) (RuntimeDescriptor, error) {
	if source == nil {
		return RuntimeDescriptor{}, errors.New("runtime verifier invalid fixture is nil")
	}
	stat, err := source.Stat()
	if err != nil {
		return RuntimeDescriptor{}, fmt.Errorf("stat runtime verifier invalid fixture: %w", err)
	}
	if !stat.Mode().IsRegular() ||
		stat.Size() != runtimeVerifierCorpusInvalidBytes {
		return RuntimeDescriptor{}, fmt.Errorf(
			"runtime verifier invalid fixture is not a %d-byte regular file",
			runtimeVerifierCorpusInvalidBytes,
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return RuntimeDescriptor{}, fmt.Errorf(
			"rewind runtime verifier invalid fixture: %w",
			err,
		)
	}
	digest := sha256.New()
	written, err := io.Copy(digest, source)
	if err != nil {
		return RuntimeDescriptor{}, fmt.Errorf(
			"hash runtime verifier invalid fixture: %w",
			err,
		)
	}
	if written != runtimeVerifierCorpusInvalidBytes {
		return RuntimeDescriptor{}, errors.New(
			"runtime verifier invalid fixture changed while hashing",
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return RuntimeDescriptor{}, fmt.Errorf(
			"rewind hashed runtime verifier invalid fixture: %w",
			err,
		)
	}
	return RuntimeDescriptor{
		Architecture:      architecture,
		Digest:            "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		FormatVersion:     RuntimeDescriptorFormatVersion,
		MediaType:         RuntimeArtifactMediaType,
		RuntimeAPIVersion: RuntimeAPIVersion,
		SizeBytes:         runtimeVerifierCorpusInvalidBytes,
	}, nil
}

func (release *RuntimeRelease) Catalog() []byte {
	if release == nil || release.closed {
		return nil
	}
	return append([]byte(nil), release.catalog...)
}

func (release *RuntimeRelease) Statement() []byte {
	if release == nil || release.closed {
		return nil
	}
	return append([]byte(nil), release.statement...)
}

func (release *RuntimeRelease) Corpus() []byte {
	if release == nil || release.closed {
		return nil
	}
	return append([]byte(nil), release.corpus...)
}

func (release *RuntimeRelease) ForEachRuntime(
	ctx context.Context,
	visit func(RuntimeDescriptor, io.Reader) error,
) error {
	if ctx == nil {
		return errors.New("runtime release iteration context is nil")
	}
	if release == nil || release.closed {
		return errors.New("runtime release is closed")
	}
	if visit == nil {
		return errors.New("runtime release visitor is nil")
	}
	catalog, err := ParseRuntimeCatalog(release.catalog)
	if err != nil {
		return err
	}
	for _, descriptor := range catalog.runtimes {
		snapshot := release.objects[descriptor.Digest]
		if snapshot == nil || snapshot.content == nil {
			return fmt.Errorf("runtime release object %q is closed", descriptor.Digest)
		}
		reader, err := snapshot.content.uploadReader(ctx)
		if err != nil {
			return err
		}
		if err := visit(descriptor, reader); err != nil {
			return err
		}
	}
	return nil
}

func (release *RuntimeRelease) ForEachToolchain(
	ctx context.Context,
	architecture RuntimeArchitecture,
	visit func(ToolObject, io.Reader) error,
) error {
	if err := ValidateRuntimeArchitecture(architecture); err != nil {
		return err
	}
	return release.forEachToolchain(ctx, architecture, visit)
}

func (release *RuntimeRelease) forEachToolchain(
	ctx context.Context,
	architecture RuntimeArchitecture,
	visit func(ToolObject, io.Reader) error,
) error {
	if ctx == nil {
		return errors.New("standard-toolchain release iteration context is nil")
	}
	if release == nil || release.closed {
		return errors.New("standard-toolchain release is closed")
	}
	if visit == nil {
		return errors.New("standard-toolchain release visitor is nil")
	}
	catalog, err := ParseToolchainCatalog(release.toolchainCatalog)
	if err != nil {
		return err
	}
	objects, err := toolchainClosureObjects(catalog, architecture)
	if err != nil {
		return err
	}
	for _, descriptor := range objects {
		snapshot := release.toolchainObjects[descriptor.Digest]
		if snapshot == nil || snapshot.content == nil {
			return fmt.Errorf(
				"standard-toolchain release object %q is closed",
				descriptor.Digest,
			)
		}
		reader, err := snapshot.content.uploadReader(ctx)
		if err != nil {
			return err
		}
		if err := visit(descriptor, reader); err != nil {
			return err
		}
	}
	return nil
}

func (release *RuntimeRelease) Close() error {
	if release == nil || release.closed {
		return nil
	}
	release.closed = true
	var closeErr error
	for digest, snapshot := range release.objects {
		closeErr = errors.Join(
			closeErr,
			closeRuntimeReleaseSnapshot(digest, snapshot),
		)
		delete(release.objects, digest)
	}
	for digest, snapshot := range release.toolchainObjects {
		closeErr = errors.Join(
			closeErr,
			closeToolchainSnapshot(digest, snapshot),
		)
		delete(release.toolchainObjects, digest)
	}
	if release.invalidFile != nil {
		closeErr = errors.Join(
			closeErr,
			closeRuntimeReleaseSnapshot(
				release.invalid.Digest,
				release.invalidFile,
			),
		)
		release.invalidFile = nil
	}
	return closeErr
}

func closeRuntimeReleaseSnapshot(
	digest string,
	snapshot *RuntimeArtifactSnapshot,
) error {
	if err := snapshot.Close(); err != nil {
		return fmt.Errorf("close runtime release object %q: %w", digest, err)
	}
	return nil
}

func WriteRuntimeReleaseDirectory(
	ctx context.Context,
	release *RuntimeRelease,
	directory string,
) (returnErr error) {
	if ctx == nil {
		return errors.New("runtime release write context is nil")
	}
	if release == nil || release.closed {
		return errors.New("runtime release is closed")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("runtime release output directory is not canonical absolute")
	}
	parent := filepath.Dir(directory)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create runtime release output parent: %w", err)
	}
	if _, err := os.Lstat(directory); err == nil {
		return errors.New("runtime release output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime release output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(directory)+"-*")
	if err != nil {
		return fmt.Errorf("create runtime release output staging: %w", err)
	}
	if err := os.Remove(staging); err != nil {
		return errors.Join(err, os.RemoveAll(staging))
	}
	defer func() {
		if staging != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(staging))
		}
	}()
	if err := writeRuntimeReleaseDirectory(ctx, release, staging); err != nil {
		return err
	}
	if err := os.Rename(staging, directory); err != nil {
		return fmt.Errorf("publish runtime release directory: %w", err)
	}
	staging = ""
	return syncRuntimeReleaseDirectory(parent)
}

func writeRuntimeReleaseDirectory(
	ctx context.Context,
	release *RuntimeRelease,
	directory string,
) error {
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create runtime release directory: %w", err)
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseCatalogFile),
		release.catalog,
	); err != nil {
		return err
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseStatementFile),
		release.statement,
	); err != nil {
		return err
	}
	if err := os.Mkdir(
		filepath.Join(directory, ToolchainReleaseDirectory),
		0o755,
	); err != nil {
		return fmt.Errorf("create standard-toolchain release directory: %w", err)
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, ToolchainReleaseCatalogFile),
		release.toolchainCatalog,
	); err != nil {
		return err
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, ToolchainReleaseStatementFile),
		release.toolchainStatement,
	); err != nil {
		return err
	}
	toolchainObjectRoot := filepath.Join(
		directory,
		ToolchainReleaseObjectsDirectory,
		"sha256",
	)
	if err := os.MkdirAll(toolchainObjectRoot, 0o755); err != nil {
		return fmt.Errorf(
			"create standard-toolchain release object directory: %w",
			err,
		)
	}
	if err := release.forEachToolchain(
		ctx,
		"",
		func(descriptor ToolObject, source io.Reader) error {
			return writeRuntimeReleaseReader(
				ctx,
				filepath.Join(
					toolchainObjectRoot,
					strings.TrimPrefix(descriptor.Digest, "sha256:"),
				),
				descriptor.SizeBytes,
				source,
			)
		},
	); err != nil {
		return err
	}
	lineage, err := CanonicalRuntimeReleaseLineage(release.lineage)
	if err != nil {
		return err
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseLineageFile),
		lineage,
	); err != nil {
		return err
	}
	if err := writeRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseCorpusFile),
		release.corpus,
	); err != nil {
		return err
	}
	if err := writeRuntimeReleaseSnapshot(
		ctx,
		filepath.Join(directory, RuntimeReleaseInvalidFile),
		release.invalidFile,
	); err != nil {
		return err
	}
	if err := writeRuntimeReleaseSnapshot(
		ctx,
		filepath.Join(directory, RuntimeReleaseValidFile),
		release.objects[release.valid.Digest],
	); err != nil {
		return err
	}
	objectRoot := filepath.Join(directory, RuntimeReleaseObjectsDirectory, "sha256")
	if err := os.MkdirAll(objectRoot, 0o755); err != nil {
		return fmt.Errorf("create runtime release object directory: %w", err)
	}
	if err := release.ForEachRuntime(
		ctx,
		func(descriptor RuntimeDescriptor, source io.Reader) error {
			return writeRuntimeReleaseReader(
				ctx,
				filepath.Join(
					objectRoot,
					strings.TrimPrefix(descriptor.Digest, "sha256:"),
				),
				descriptor.SizeBytes,
				source,
			)
		},
	); err != nil {
		return err
	}
	return syncRuntimeReleaseDirectory(directory)
}

func LoadRuntimeReleaseDirectory(
	ctx context.Context,
	directory,
	scratchDirectory,
	unitCgroupRoot string,
	releaseTag string,
	architecture RuntimeArchitecture,
) (_ *RuntimeRelease, _ []byte, _ []byte, returnErr error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, nil, nil, errors.New(
			"runtime release input directory is not canonical absolute",
		)
	}
	predecessor, err := openRuntimeReleaseDirectory(directory, releaseTag)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, predecessor.Close())
	}()
	catalog, err := VerifyRuntimeCatalogForRelease(
		predecessor.Catalog,
		predecessor.Bundle,
		predecessor.TrustedRoot,
		predecessor.Lineage.Release,
		predecessor.Lineage.Predecessor,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	toolchainCatalog, err := VerifyToolchainCatalogForRelease(
		predecessor.ToolchainCatalog,
		predecessor.ToolchainBundle,
		predecessor.ToolchainTrustedRoot,
		predecessor.Lineage.Release,
		predecessor.Lineage.Predecessor,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	corpusBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseCorpusFile),
		maxRuntimeVerifierCorpusManifestBytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	corpus, err := parseRuntimeVerifierCorpusManifest(
		corpusBytes,
		catalog,
		architecture,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	valid, err := OpenRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseValidFile),
		corpus.Valid.Descriptor.SizeBytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, valid.Close())
	}()
	invalid, err := OpenRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseInvalidFile),
		corpus.Invalid.Descriptor.SizeBytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, invalid.Close())
	}()
	release, err := prepareRuntimeRelease(ctx, RuntimeReleaseSource{
		Runtime:          valid,
		Invalid:          invalid,
		Descriptor:       corpus.Valid.Descriptor,
		ScratchDirectory: scratchDirectory,
		UnitCgroupRoot:   unitCgroupRoot,
		Lineage:          predecessor.Lineage,
		Predecessor:      predecessor,
	},
		catalog,
		toolchainCatalog,
		VerifyRuntimeArtifact,
		snapshotRuntimeReleaseFile,
		snapshotToolchain,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, release.Close())
		}
	}()
	statementBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, RuntimeReleaseStatementFile),
		maxReleaseBundleBytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if !bytes.Equal(release.catalog, predecessor.Catalog) {
		return nil, nil, nil, errors.New(
			"runtime release catalog changed while loading captured inputs",
		)
	}
	if !bytes.Equal(release.statement, statementBytes) {
		return nil, nil, nil, errors.New(
			"runtime release attestation statement changed while loading captured inputs",
		)
	}
	if !bytes.Equal(release.corpus, corpusBytes) {
		return nil, nil, nil, errors.New(
			"runtime release verifier corpus changed while loading captured inputs",
		)
	}
	toolchainStatementBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseStatementFile),
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if !bytes.Equal(release.toolchainCatalog, predecessor.ToolchainCatalog) {
		return nil, nil, nil, errors.New(
			"standard-toolchain release catalog changed while loading captured inputs",
		)
	}
	if !bytes.Equal(release.toolchainStatement, toolchainStatementBytes) {
		return nil, nil, nil, errors.New(
			"standard-toolchain release attestation statement changed while loading captured inputs",
		)
	}
	release.toolchainBundle = append(
		[]byte(nil),
		predecessor.ToolchainBundle...,
	)
	release.toolchainTrustedRoot = append(
		[]byte(nil),
		predecessor.ToolchainTrustedRoot...,
	)
	return release,
		append([]byte(nil), predecessor.Bundle...),
		append([]byte(nil), predecessor.TrustedRoot...),
		nil
}

func writeRuntimeReleaseFile(path string, raw []byte) error {
	return writeRuntimeReleaseReader(
		context.Background(),
		path,
		int64(len(raw)),
		bytes.NewReader(raw),
	)
}

func writeRuntimeReleaseSnapshot(
	ctx context.Context,
	path string,
	snapshot *RuntimeArtifactSnapshot,
) error {
	if snapshot == nil || snapshot.content == nil {
		return errors.New("runtime release snapshot is closed")
	}
	reader, err := snapshot.content.uploadReader(ctx)
	if err != nil {
		return err
	}
	return writeRuntimeReleaseReader(
		ctx,
		path,
		snapshot.descriptor.SizeBytes,
		reader,
	)
}

func writeRuntimeReleaseReader(
	ctx context.Context,
	path string,
	sizeBytes int64,
	source io.Reader,
) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create runtime release file %q: %w", path, err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	written, err := copyRuntimeRelease(ctx, file, source)
	if err != nil {
		return fmt.Errorf("write runtime release file %q: %w", path, err)
	}
	if written != sizeBytes {
		return fmt.Errorf(
			"runtime release file %q size = %d, want %d",
			path,
			written,
			sizeBytes,
		)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync runtime release file %q: %w", path, err)
	}
	if err := file.Chmod(runtimeReleaseDirectoryFileMode); err != nil {
		return fmt.Errorf("set runtime release file %q mode: %w", path, err)
	}
	return nil
}

func syncRuntimeReleaseDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open runtime release directory: %w", err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync runtime release directory: %w", err)
	}
	return nil
}

var runtimeReleaseArchiveFiles = []string{
	RuntimeReleaseStatementFile,
	RuntimeReleaseCatalogFile,
	RuntimeReleaseBundleFile,
	RuntimeReleaseLineageFile,
	RuntimeReleaseTrustedRootFile,
	RuntimeReleaseCorpusFile,
	RuntimeReleaseInvalidFile,
	RuntimeReleaseValidFile,
	ToolchainReleaseStatementFile,
	ToolchainReleaseCatalogFile,
	ToolchainReleaseBundleFile,
	ToolchainReleaseTrustedRootFile,
}

type runtimeReleaseArchiveMember struct {
	name      string
	sizeBytes int64
	source    func(context.Context) (io.Reader, error)
}

func WriteRuntimeReleaseArchive(
	ctx context.Context,
	release *RuntimeRelease,
	bundle,
	trustedRoot []byte,
	scratchDirectory string,
	destination io.Writer,
) (_ RuntimeReleasePackage, returnErr error) {
	return writeRuntimeReleaseArchive(
		ctx,
		release,
		bundle,
		trustedRoot,
		scratchDirectory,
		destination,
		runtimeReleaseCatalogAuthenticator(release.lineage),
		toolchainReleaseCatalogAuthenticator(release.lineage),
		snapshotRuntimeReleaseFile,
		snapshotToolchain,
	)
}

func writeRuntimeReleaseArchive(
	ctx context.Context,
	release *RuntimeRelease,
	bundle,
	trustedRoot []byte,
	scratchDirectory string,
	destination io.Writer,
	authenticate runtimeCatalogAuthenticator,
	authenticateToolchain toolchainCatalogAuthenticator,
	snapshot runtimeReleaseSnapshotter,
	snapshotToolchainClosure toolchainReleaseSnapshotter,
) (_ RuntimeReleasePackage, returnErr error) {
	if release == nil || release.closed {
		return RuntimeReleasePackage{}, errors.New("runtime release is closed")
	}
	if authenticate == nil ||
		authenticateToolchain == nil ||
		snapshot == nil ||
		snapshotToolchainClosure == nil {
		return RuntimeReleasePackage{}, errors.New(
			"runtime release archive authenticator is nil",
		)
	}
	if _, err := authenticate(
		release.catalog,
		bundle,
		trustedRoot,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if !bytes.Equal(trustedRoot, release.toolchainTrustedRoot) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime and standard-toolchain trusted roots do not exact-match",
		)
	}
	if _, err := authenticateToolchain(
		release.toolchainCatalog,
		release.toolchainBundle,
		release.toolchainTrustedRoot,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	lineage, err := CanonicalRuntimeReleaseLineage(release.lineage)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	values := map[string][]byte{
		RuntimeReleaseStatementFile:     release.statement,
		RuntimeReleaseCatalogFile:       release.catalog,
		RuntimeReleaseBundleFile:        bundle,
		RuntimeReleaseLineageFile:       lineage,
		RuntimeReleaseTrustedRootFile:   trustedRoot,
		RuntimeReleaseCorpusFile:        release.corpus,
		ToolchainReleaseStatementFile:   release.toolchainStatement,
		ToolchainReleaseCatalogFile:     release.toolchainCatalog,
		ToolchainReleaseBundleFile:      release.toolchainBundle,
		ToolchainReleaseTrustedRootFile: release.toolchainTrustedRoot,
	}
	members := make(
		[]runtimeReleaseArchiveMember,
		0,
		len(values)+2+len(release.objects)+len(release.toolchainObjects),
	)
	for name, raw := range values {
		value := append([]byte(nil), raw...)
		members = append(members, runtimeReleaseArchiveMember{
			name:      name,
			sizeBytes: int64(len(value)),
			source: func(context.Context) (io.Reader, error) {
				return bytes.NewReader(value), nil
			},
		})
	}
	for name, snapshot := range map[string]*RuntimeArtifactSnapshot{
		RuntimeReleaseValidFile:   release.objects[release.valid.Digest],
		RuntimeReleaseInvalidFile: release.invalidFile,
	} {
		captured := snapshot
		members = append(members, runtimeReleaseArchiveMember{
			name:      name,
			sizeBytes: captured.descriptor.SizeBytes,
			source: func(ctx context.Context) (io.Reader, error) {
				return captured.content.uploadReader(ctx)
			},
		})
	}
	for digest, snapshot := range release.objects {
		captured := snapshot
		members = append(members, runtimeReleaseArchiveMember{
			name:      "objects/sha256/" + strings.TrimPrefix(digest, "sha256:"),
			sizeBytes: captured.descriptor.SizeBytes,
			source: func(ctx context.Context) (io.Reader, error) {
				return captured.content.uploadReader(ctx)
			},
		})
	}
	for digest, snapshot := range release.toolchainObjects {
		captured := snapshot
		members = append(members, runtimeReleaseArchiveMember{
			name: ToolchainReleaseObjectsDirectory + "/sha256/" +
				strings.TrimPrefix(digest, "sha256:"),
			sizeBytes: captured.descriptor.SizeBytes,
			source: func(ctx context.Context) (io.Reader, error) {
				return captured.content.uploadReader(ctx)
			},
		})
	}
	sort.Slice(members, func(first, second int) bool {
		return members[first].name < members[second].name
	})
	archive, err := os.CreateTemp(scratchDirectory, ".helmr-runtime-release-*")
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, archive.Close(), os.Remove(archive.Name()))
	}()
	writer := tar.NewWriter(archive)
	for _, member := range members {
		if err := writeRuntimeReleaseTarHeader(
			writer,
			member.name,
			member.sizeBytes,
		); err != nil {
			return RuntimeReleasePackage{}, err
		}
		reader, err := member.source(ctx)
		if err != nil {
			return RuntimeReleasePackage{}, err
		}
		written, err := copyRuntimeRelease(ctx, writer, reader)
		if err != nil {
			return RuntimeReleasePackage{}, err
		}
		if written != member.sizeBytes {
			return RuntimeReleasePackage{}, fmt.Errorf(
				"runtime release archive member %q size = %d, want %d",
				member.name,
				written,
				member.sizeBytes,
			)
		}
	}
	if err := writer.Close(); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if err := archive.Sync(); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, err
	}
	digest := sha256.New()
	sizeBytes, err := copyRuntimeRelease(ctx, digest, archive)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	result := RuntimeReleasePackage{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		SizeBytes: sizeBytes,
	}
	verified, err := openRuntimeReleaseArchive(
		ctx,
		archive.Name(),
		RuntimeReleaseRef{
			Release:   release.lineage.Release,
			Digest:    result.Digest,
			SizeBytes: result.SizeBytes,
		},
		scratchDirectory,
		func(
			_ RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*RuntimeCatalog, error) {
			return authenticate(catalog, bundle, trustedRoot)
		},
		func(
			_ RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*ToolchainCatalog, error) {
			return authenticateToolchain(catalog, bundle, trustedRoot)
		},
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	verifiedCatalog, err := authenticate(
		verified.Catalog,
		verified.Bundle,
		verified.TrustedRoot,
	)
	verifiedToolchainCatalog, toolchainErr := authenticateToolchain(
		verified.ToolchainCatalog,
		verified.ToolchainBundle,
		verified.ToolchainTrustedRoot,
	)
	err = errors.Join(err, toolchainErr)
	objects := make(map[string]*RuntimeArtifactSnapshot)
	if err == nil {
		err = capturePredecessorRuntimeObjects(
			ctx,
			scratchDirectory,
			verified,
			verifiedCatalog,
			objects,
			snapshot,
		)
	}
	for _, object := range objects {
		err = errors.Join(err, object.Close())
	}
	toolchainObjects := make(map[string]*toolchainSnapshot)
	if err == nil {
		err = capturePredecessorToolchainObjects(
			ctx,
			scratchDirectory,
			verified,
			verifiedToolchainCatalog,
			toolchainObjects,
			snapshotToolchainClosure,
		)
	}
	for _, object := range toolchainObjects {
		err = errors.Join(err, object.Close())
	}
	if err == nil {
		corpusBytes, readErr := readRuntimeReleaseInputFile(
			filepath.Join(verified.directory, RuntimeReleaseCorpusFile),
			maxRuntimeVerifierCorpusManifestBytes,
		)
		if readErr != nil {
			err = readErr
		} else {
			corpus, parseErr := parseRuntimeVerifierCorpusManifest(
				corpusBytes,
				verifiedCatalog,
				release.architecture,
			)
			if parseErr != nil {
				err = parseErr
			} else {
				for path, descriptor := range map[string]RuntimeDescriptor{
					RuntimeReleaseValidFile:   corpus.Valid.Descriptor,
					RuntimeReleaseInvalidFile: corpus.Invalid.Descriptor,
				} {
					file, openErr := OpenRuntimeReleaseFile(
						filepath.Join(verified.directory, path),
						descriptor.SizeBytes,
					)
					if openErr != nil {
						err = openErr
						break
					}
					fixture, snapshotErr := snapshot(
						ctx,
						scratchDirectory,
						descriptor,
						file,
					)
					err = errors.Join(snapshotErr, file.Close())
					if fixture != nil {
						err = errors.Join(err, fixture.Close())
					}
					if err != nil {
						break
					}
				}
			}
		}
	}
	err = errors.Join(err, verified.Close())
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if written, err := copyRuntimeRelease(ctx, destination, archive); err != nil {
		return RuntimeReleasePackage{}, err
	} else if written != result.SizeBytes {
		return RuntimeReleasePackage{}, errors.New(
			"verified runtime release archive changed before publication",
		)
	}
	return result, nil
}

func OpenRuntimeReleaseArchive(
	ctx context.Context,
	path string,
	expected RuntimeReleaseRef,
	scratchDirectory string,
) (_ *RuntimeReleasePredecessor, returnErr error) {
	return openRuntimeReleaseArchive(
		ctx,
		path,
		expected,
		scratchDirectory,
		func(
			lineage RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*RuntimeCatalog, error) {
			return VerifyRuntimeCatalogForRelease(
				catalog,
				bundle,
				trustedRoot,
				lineage.Release,
				lineage.Predecessor,
			)
		},
		func(
			lineage RuntimeReleaseLineage,
			catalog,
			bundle,
			trustedRoot []byte,
		) (*ToolchainCatalog, error) {
			return VerifyToolchainCatalogForRelease(
				catalog,
				bundle,
				trustedRoot,
				lineage.Release,
				lineage.Predecessor,
			)
		},
	)
}

func openRuntimeReleaseArchive(
	ctx context.Context,
	path string,
	expected RuntimeReleaseRef,
	scratchDirectory string,
	authenticate runtimeReleaseLineageAuthenticator,
	authenticateToolchain toolchainReleaseLineageAuthenticator,
) (_ *RuntimeReleasePredecessor, returnErr error) {
	if err := ValidateRuntimeReleaseRef(expected); err != nil {
		return nil, err
	}
	if authenticate == nil || authenticateToolchain == nil {
		return nil, errors.New("runtime release archive authenticator is nil")
	}
	source, err := OpenRuntimeReleaseFile(path, expected.SizeBytes)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil || stat.Size() != expected.SizeBytes {
		return nil, errors.New("runtime release archive length does not match lineage")
	}
	snapshot, err := os.CreateTemp(scratchDirectory, ".helmr-runtime-release-input-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshot.Close(), os.Remove(snapshot.Name()))
	}()
	digest := sha256.New()
	written, err := copyRuntimeRelease(ctx, io.MultiWriter(snapshot, digest), source)
	if err != nil {
		return nil, err
	}
	actual := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if written != expected.SizeBytes || actual != expected.Digest {
		return nil, errors.New(
			"runtime release archive does not exact-match predecessor lineage",
		)
	}
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(scratchDirectory, ".helmr-runtime-release-files-*")
	if err != nil {
		return nil, err
	}
	if err := extractRuntimeReleaseArchive(snapshot, directory); err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	predecessor, err := openRuntimeReleaseDirectoryWithAuthenticators(
		directory,
		expected.Release,
		authenticate,
		authenticateToolchain,
	)
	if err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	predecessor.directory = directory
	return predecessor, nil
}

func VerifyRuntimeReleaseArchive(
	ctx context.Context,
	source *os.File,
	lineage RuntimeReleaseLineage,
	trustedRoot []byte,
	scratchDirectory,
	unitCgroupRoot,
	outputDirectory string,
) (_ RuntimeReleasePackage, returnErr error) {
	if err := ValidateRuntimeReleaseLineage(lineage, lineage.Release); err != nil {
		return RuntimeReleasePackage{}, err
	}
	stat, err := source.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 {
		return RuntimeReleasePackage{}, errors.New(
			"runtime release archive source is not a non-empty regular file",
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, err
	}
	snapshot, err := os.CreateTemp(scratchDirectory, ".helmr-runtime-release-verify-*")
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshot.Close(), os.Remove(snapshot.Name()))
	}()
	digest := sha256.New()
	sizeBytes, err := copyRuntimeRelease(
		ctx,
		io.MultiWriter(snapshot, digest),
		source,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	reference := RuntimeReleaseRef{
		Release:   lineage.Release,
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		SizeBytes: sizeBytes,
	}
	if err := snapshot.Sync(); err != nil {
		return RuntimeReleasePackage{}, err
	}
	predecessor, err := openRuntimeReleaseArchive(
		ctx,
		snapshot.Name(),
		reference,
		scratchDirectory,
		func(
			embeddedLineage RuntimeReleaseLineage,
			catalog,
			bundle,
			embeddedTrustedRoot []byte,
		) (*RuntimeCatalog, error) {
			return pinnedRuntimeCatalogAuthenticator(
				embeddedLineage,
				trustedRoot,
			)(catalog, bundle, embeddedTrustedRoot)
		},
		func(
			embeddedLineage RuntimeReleaseLineage,
			catalog,
			bundle,
			embeddedTrustedRoot []byte,
		) (*ToolchainCatalog, error) {
			return pinnedToolchainCatalogAuthenticator(
				embeddedLineage,
				trustedRoot,
			)(catalog, bundle, embeddedTrustedRoot)
		},
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, predecessor.Close())
	}()
	wantLineage, _ := CanonicalRuntimeReleaseLineage(lineage)
	gotLineage, _ := CanonicalRuntimeReleaseLineage(predecessor.Lineage)
	if !bytes.Equal(gotLineage, wantLineage) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime release archive lineage does not exact-match repository descriptor",
		)
	}
	release, _, _, err := LoadRuntimeReleaseDirectory(
		ctx,
		predecessor.directory,
		scratchDirectory,
		unitCgroupRoot,
		lineage.Release,
		ArchitectureX8664,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	if err := release.Close(); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if outputDirectory != "" {
		if !filepath.IsAbs(outputDirectory) ||
			filepath.Clean(outputDirectory) != outputDirectory {
			return RuntimeReleasePackage{}, errors.New(
				"runtime release archive output is not canonical absolute",
			)
		}
		if _, err := os.Lstat(outputDirectory); err == nil {
			return RuntimeReleasePackage{}, errors.New(
				"runtime release archive output already exists",
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return RuntimeReleasePackage{}, err
		}
		if err := os.Rename(predecessor.directory, outputDirectory); err != nil {
			return RuntimeReleasePackage{}, err
		}
		predecessor.directory = ""
	}
	return RuntimeReleasePackage{
		Digest:    reference.Digest,
		SizeBytes: reference.SizeBytes,
	}, nil
}

func extractRuntimeReleaseArchive(source io.Reader, directory string) error {
	counted := &runtimeReleaseCountingReader{source: source}
	reader := tar.NewReader(counted)
	seen := make(map[string]bool)
	var prior string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Name <= prior || seen[header.Name] {
			return errors.New("runtime release archive members are not unique and sorted")
		}
		prior = header.Name
		seen[header.Name] = true
		if err := validateRuntimeReleaseArchiveName(header.Name); err != nil {
			return err
		}
		if err := validateRuntimeReleaseTarHeader(header); err != nil {
			return err
		}
		path := filepath.Join(directory, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeRuntimeReleaseReader(
			context.Background(),
			path,
			header.Size,
			reader,
		); err != nil {
			return err
		}
	}
	for _, name := range runtimeReleaseArchiveFiles {
		if !seen[name] {
			return fmt.Errorf("runtime release archive omits %q", name)
		}
	}
	var trailing [1]byte
	if count, err := counted.Read(trailing[:]); count != 0 || !errors.Is(err, io.EOF) {
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read runtime release archive trailing bytes: %w", err)
		}
		return errors.New("runtime release archive contains trailing bytes")
	}
	return nil
}

func validateRuntimeReleaseArchiveName(name string) error {
	for _, fixed := range runtimeReleaseArchiveFiles {
		if name == fixed {
			return nil
		}
	}
	prefixes := []string{
		RuntimeReleaseObjectsDirectory + "/sha256/",
		ToolchainReleaseObjectsDirectory + "/sha256/",
	}
	var digest string
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			digest = strings.TrimPrefix(name, prefix)
			break
		}
	}
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("runtime release archive contains unexpected member %q", name)
	}
	if _, err := hex.DecodeString(digest); err != nil ||
		digest != strings.ToLower(digest) {
		return fmt.Errorf("runtime release archive contains invalid object %q", name)
	}
	return nil
}

func WriteRuntimeWorkerPackage(
	ctx context.Context,
	release *RuntimeRelease,
	bundle,
	trustedRoot []byte,
	scratchDirectory string,
	destination io.Writer,
) (RuntimeReleasePackage, error) {
	authenticate := runtimeReleaseCatalogAuthenticator(release.lineage)
	return writeRuntimeWorkerPackage(
		ctx,
		release,
		bundle,
		trustedRoot,
		scratchDirectory,
		destination,
		authenticate,
		toolchainReleaseCatalogAuthenticator(release.lineage),
	)
}

func writeRuntimeWorkerPackage(
	ctx context.Context,
	release *RuntimeRelease,
	bundle,
	trustedRoot []byte,
	scratchDirectory string,
	destination io.Writer,
	authenticate runtimeCatalogAuthenticator,
	authenticateToolchain toolchainCatalogAuthenticator,
) (_ RuntimeReleasePackage, returnErr error) {
	if ctx == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package context is nil")
	}
	if release == nil || release.closed {
		return RuntimeReleasePackage{}, errors.New("runtime release is closed")
	}
	if destination == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package destination is nil")
	}
	if authenticate == nil || authenticateToolchain == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package authenticator is nil")
	}
	catalog, err := authenticate(release.catalog, bundle, trustedRoot)
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("authenticate runtime package catalog: %w", err)
	}
	if _, err := parseRuntimeVerifierCorpusManifest(
		release.corpus,
		catalog,
		release.architecture,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if !bytes.Equal(trustedRoot, release.toolchainTrustedRoot) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime and standard-toolchain trusted roots do not exact-match",
		)
	}
	if _, err := authenticateToolchain(
		release.toolchainCatalog,
		release.toolchainBundle,
		release.toolchainTrustedRoot,
	); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf(
			"authenticate runtime package standard-toolchain catalog: %w",
			err,
		)
	}
	if len(bundle) == 0 || int64(len(bundle)) > maxReleaseBundleBytes {
		return RuntimeReleasePackage{}, errors.New("runtime package bundle size is invalid")
	}
	if len(trustedRoot) == 0 || int64(len(trustedRoot)) > maxReleaseTrustedRootBytes {
		return RuntimeReleasePackage{}, errors.New("runtime package trusted root size is invalid")
	}

	archive, err := os.CreateTemp(scratchDirectory, ".helmr-runtime-package-*")
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("create runtime package snapshot: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, archive.Close())
		returnErr = errors.Join(returnErr, os.Remove(archive.Name()))
	}()
	if err := archive.Chmod(0o400); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("set runtime package snapshot mode: %w", err)
	}
	if err := writeRuntimeWorkerPackageArchive(
		ctx,
		archive,
		release,
		bundle,
		trustedRoot,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if err := archive.Sync(); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("sync runtime package snapshot: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("rewind runtime package snapshot: %w", err)
	}
	if err := validateRuntimeWorkerPackage(
		archive,
		release.architecture,
		authenticate,
		authenticateToolchain,
	); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("verify composed runtime package: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("rewind verified runtime package: %w", err)
	}
	digest := sha256.New()
	sizeBytes, err := copyRuntimeRelease(
		ctx,
		io.MultiWriter(destination, digest),
		archive,
	)
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("publish verified runtime package: %w", err)
	}
	return RuntimeReleasePackage{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		SizeBytes: sizeBytes,
	}, nil
}

func writeRuntimeWorkerPackageArchive(
	ctx context.Context,
	destination io.Writer,
	release *RuntimeRelease,
	bundle,
	trustedRoot []byte,
) error {
	writer := tar.NewWriter(destination)
	members := map[string][]byte{
		RuntimeReleaseCatalogFile:       release.catalog,
		RuntimeReleaseBundleFile:        bundle,
		RuntimeReleaseTrustedRootFile:   trustedRoot,
		RuntimeReleaseCorpusFile:        release.corpus,
		ToolchainReleaseCatalogFile:     release.toolchainCatalog,
		ToolchainReleaseBundleFile:      release.toolchainBundle,
		ToolchainReleaseTrustedRootFile: release.toolchainTrustedRoot,
	}
	for _, name := range runtimeReleasePackageFiles {
		if name == RuntimeReleaseValidFile || name == RuntimeReleaseInvalidFile {
			snapshot := release.invalidFile
			if name == RuntimeReleaseValidFile {
				snapshot = release.objects[release.valid.Digest]
			}
			if snapshot == nil || snapshot.content == nil {
				return errors.New("runtime package valid fixture is closed")
			}
			if err := writeRuntimeReleaseTarHeader(
				writer,
				name,
				snapshot.descriptor.SizeBytes,
			); err != nil {
				return err
			}
			reader, err := snapshot.content.uploadReader(ctx)
			if err != nil {
				return err
			}
			written, err := copyRuntimeRelease(ctx, writer, reader)
			if err != nil {
				return err
			}
			if written != snapshot.descriptor.SizeBytes {
				return fmt.Errorf(
					"runtime package member %q size = %d, want %d",
					name,
					written,
					snapshot.descriptor.SizeBytes,
				)
			}
			continue
		}
		raw := members[name]
		if err := writeRuntimeReleaseTarHeader(writer, name, int64(len(raw))); err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return fmt.Errorf("write runtime package member %q: %w", name, err)
		}
	}
	if err := release.ForEachToolchain(
		ctx,
		release.architecture,
		func(descriptor ToolObject, source io.Reader) error {
			name := ToolchainReleaseObjectsDirectory + "/sha256/" +
				strings.TrimPrefix(descriptor.Digest, "sha256:")
			if err := writeRuntimeReleaseTarHeader(
				writer,
				name,
				descriptor.SizeBytes,
			); err != nil {
				return err
			}
			written, err := copyRuntimeRelease(ctx, writer, source)
			if err != nil {
				return err
			}
			if written != descriptor.SizeBytes {
				return fmt.Errorf(
					"runtime package member %q size = %d, want %d",
					name,
					written,
					descriptor.SizeBytes,
				)
			}
			return nil
		},
	); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close runtime package: %w", err)
	}
	return nil
}

func writeRuntimeReleaseTarHeader(
	writer *tar.Writer,
	name string,
	sizeBytes int64,
) error {
	epoch := time.Unix(0, 0).UTC()
	header := &tar.Header{
		Name:     name,
		Mode:     runtimeReleasePackageFileMode,
		Uid:      0,
		Gid:      0,
		Size:     sizeBytes,
		ModTime:  epoch,
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write runtime package member %q header: %w", name, err)
	}
	return nil
}

func ValidateRuntimeWorkerPackage(
	source io.Reader,
	architecture RuntimeArchitecture,
	lineage RuntimeReleaseLineage,
) error {
	return validateRuntimeWorkerPackage(
		source,
		architecture,
		runtimeReleaseCatalogAuthenticator(lineage),
		toolchainReleaseCatalogAuthenticator(lineage),
	)
}

func validateRuntimeWorkerPackage(
	source io.Reader,
	architecture RuntimeArchitecture,
	authenticate runtimeCatalogAuthenticator,
	authenticateToolchain toolchainCatalogAuthenticator,
) error {
	if source == nil {
		return errors.New("runtime package source is nil")
	}
	if err := ValidateRuntimeArchitecture(architecture); err != nil {
		return err
	}
	if authenticate == nil || authenticateToolchain == nil {
		return errors.New("runtime package authenticator is nil")
	}
	counted := &runtimeReleaseCountingReader{
		source: io.LimitReader(source, maxRuntimeReleasePackageBytes+1),
	}
	reader := tar.NewReader(counted)
	small := make(map[string][]byte, len(runtimeReleasePackageFiles)-1)
	var validDigest string
	var validSize int64
	for position, wantName := range runtimeReleasePackageFiles {
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf(
				"read runtime package member %q: %w",
				wantName,
				err,
			)
		}
		if header.Name != wantName {
			return fmt.Errorf(
				"runtime package member %d = %q, want %q",
				position,
				header.Name,
				wantName,
			)
		}
		if err := validateRuntimeReleaseTarHeader(header); err != nil {
			return fmt.Errorf("runtime package member %q: %w", header.Name, err)
		}
		if header.Name == RuntimeReleaseValidFile {
			digest := sha256.New()
			written, err := io.Copy(digest, reader)
			if err != nil {
				return fmt.Errorf("hash runtime package valid fixture: %w", err)
			}
			validDigest = "sha256:" + hex.EncodeToString(digest.Sum(nil))
			validSize = written
			continue
		}
		limit, err := runtimeReleasePackageMemberLimit(header.Name)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return fmt.Errorf("read runtime package member %q: %w", header.Name, err)
		}
		if int64(len(raw)) != header.Size || int64(len(raw)) > limit {
			return fmt.Errorf("runtime package member %q has invalid size", header.Name)
		}
		small[header.Name] = raw
	}
	catalog, err := authenticate(
		small[RuntimeReleaseCatalogFile],
		small[RuntimeReleaseBundleFile],
		small[RuntimeReleaseTrustedRootFile],
	)
	if err != nil {
		return fmt.Errorf("authenticate runtime package catalog: %w", err)
	}
	if !bytes.Equal(
		small[RuntimeReleaseTrustedRootFile],
		small[ToolchainReleaseTrustedRootFile],
	) {
		return errors.New(
			"runtime and standard-toolchain trusted roots do not exact-match",
		)
	}
	toolchainCatalog, err := authenticateToolchain(
		small[ToolchainReleaseCatalogFile],
		small[ToolchainReleaseBundleFile],
		small[ToolchainReleaseTrustedRootFile],
	)
	if err != nil {
		return fmt.Errorf(
			"authenticate runtime package standard-toolchain catalog: %w",
			err,
		)
	}
	toolchainObjects, err := toolchainClosureObjects(
		toolchainCatalog,
		architecture,
	)
	if err != nil {
		return err
	}
	for _, descriptor := range toolchainObjects {
		wantName := ToolchainReleaseObjectsDirectory + "/sha256/" +
			strings.TrimPrefix(descriptor.Digest, "sha256:")
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf(
				"read runtime package member %q: %w",
				wantName,
				err,
			)
		}
		if header.Name != wantName {
			return fmt.Errorf(
				"runtime package standard-toolchain member = %q, want %q",
				header.Name,
				wantName,
			)
		}
		if err := validateRuntimeReleaseTarHeader(header); err != nil {
			return fmt.Errorf("runtime package member %q: %w", header.Name, err)
		}
		if header.Size != descriptor.SizeBytes {
			return fmt.Errorf(
				"runtime package member %q size = %d, want %d",
				header.Name,
				header.Size,
				descriptor.SizeBytes,
			)
		}
		digest := sha256.New()
		written, err := io.Copy(digest, reader)
		if err != nil {
			return fmt.Errorf(
				"hash runtime package member %q: %w",
				header.Name,
				err,
			)
		}
		actual := "sha256:" + hex.EncodeToString(digest.Sum(nil))
		if written != descriptor.SizeBytes || actual != descriptor.Digest {
			return fmt.Errorf(
				"runtime package member %q does not match its catalog descriptor",
				header.Name,
			)
		}
	}
	if header, err := reader.Next(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("read runtime package trailing member: %w", err)
		}
		return fmt.Errorf("runtime package contains extra member %q", header.Name)
	}
	var trailing [1]byte
	if count, err := counted.Read(trailing[:]); count != 0 || !errors.Is(err, io.EOF) {
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read runtime package trailing bytes: %w", err)
		}
		return errors.New("runtime package contains trailing bytes")
	}
	if counted.count > maxRuntimeReleasePackageBytes {
		return fmt.Errorf(
			"runtime package exceeds %d bytes",
			maxRuntimeReleasePackageBytes,
		)
	}
	document, err := parseRuntimeVerifierCorpusManifest(
		small[RuntimeReleaseCorpusFile],
		catalog,
		architecture,
	)
	if err != nil {
		return err
	}
	if validSize != document.Valid.Descriptor.SizeBytes ||
		validDigest != document.Valid.Descriptor.Digest {
		return errors.New("runtime package valid fixture does not match its descriptor")
	}
	invalidRaw := small[RuntimeReleaseInvalidFile]
	invalidDigest := sha256.Sum256(invalidRaw)
	if int64(len(invalidRaw)) != document.Invalid.Descriptor.SizeBytes ||
		"sha256:"+hex.EncodeToString(invalidDigest[:]) != document.Invalid.Descriptor.Digest {
		return errors.New("runtime package invalid fixture does not match its descriptor")
	}
	return nil
}

func VerifyRuntimeWorkerPackage(
	ctx context.Context,
	source *os.File,
	architecture RuntimeArchitecture,
	lineage RuntimeReleaseLineage,
	trustedRoot []byte,
	unitCgroupRoot,
	scratchDirectory,
	outputDirectory,
	snapshotPath string,
) (_ RuntimeReleasePackage, returnErr error) {
	return verifyRuntimeWorkerPackage(
		ctx,
		source,
		architecture,
		unitCgroupRoot,
		scratchDirectory,
		outputDirectory,
		snapshotPath,
		pinnedRuntimeCatalogAuthenticator(lineage, trustedRoot),
		pinnedToolchainCatalogAuthenticator(lineage, trustedRoot),
		VerifyRuntimeArtifact,
		snapshotRuntimeReleaseFile,
	)
}

func pinnedRuntimeCatalogAuthenticator(
	lineage RuntimeReleaseLineage,
	trustedRoot []byte,
) runtimeCatalogAuthenticator {
	return func(catalog, bundle, embeddedTrustedRoot []byte) (*RuntimeCatalog, error) {
		if !bytes.Equal(embeddedTrustedRoot, trustedRoot) {
			return nil, errors.New(
				"runtime release trusted root does not exact-match pinned release root",
			)
		}
		return VerifyRuntimeCatalogForRelease(
			catalog,
			bundle,
			trustedRoot,
			lineage.Release,
			lineage.Predecessor,
		)
	}
}

func runtimeReleaseCatalogAuthenticator(
	lineage RuntimeReleaseLineage,
) runtimeCatalogAuthenticator {
	return func(catalog, bundle, trustedRoot []byte) (*RuntimeCatalog, error) {
		return VerifyRuntimeCatalogForRelease(
			catalog,
			bundle,
			trustedRoot,
			lineage.Release,
			lineage.Predecessor,
		)
	}
}

func pinnedToolchainCatalogAuthenticator(
	lineage RuntimeReleaseLineage,
	trustedRoot []byte,
) toolchainCatalogAuthenticator {
	return func(catalog, bundle, embeddedTrustedRoot []byte) (*ToolchainCatalog, error) {
		if !bytes.Equal(embeddedTrustedRoot, trustedRoot) {
			return nil, errors.New(
				"standard-toolchain release trusted root does not exact-match pinned release root",
			)
		}
		return VerifyToolchainCatalogForRelease(
			catalog,
			bundle,
			trustedRoot,
			lineage.Release,
			lineage.Predecessor,
		)
	}
}

func toolchainReleaseCatalogAuthenticator(
	lineage RuntimeReleaseLineage,
) toolchainCatalogAuthenticator {
	return func(catalog, bundle, trustedRoot []byte) (*ToolchainCatalog, error) {
		return VerifyToolchainCatalogForRelease(
			catalog,
			bundle,
			trustedRoot,
			lineage.Release,
			lineage.Predecessor,
		)
	}
}

func verifyRuntimeWorkerPackage(
	ctx context.Context,
	source *os.File,
	architecture RuntimeArchitecture,
	unitCgroupRoot,
	scratchDirectory,
	outputDirectory,
	snapshotPath string,
	authenticate runtimeCatalogAuthenticator,
	authenticateToolchain toolchainCatalogAuthenticator,
	verify runtimeReleaseVerifier,
	snapshot runtimeReleaseSnapshotter,
) (_ RuntimeReleasePackage, returnErr error) {
	if ctx == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package context is nil")
	}
	if source == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package source is nil")
	}
	if unitCgroupRoot == "" {
		return RuntimeReleasePackage{}, errors.New("runtime package verifier cgroup root is empty")
	}
	if scratchDirectory == "" {
		return RuntimeReleasePackage{}, errors.New("runtime package scratch directory is empty")
	}
	if verify == nil || snapshot == nil {
		return RuntimeReleasePackage{}, errors.New("runtime package operations are nil")
	}
	if outputDirectory != "" &&
		(!filepath.IsAbs(outputDirectory) ||
			filepath.Clean(outputDirectory) != outputDirectory) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime package output directory is not canonical absolute",
		)
	}
	if snapshotPath != "" &&
		(!filepath.IsAbs(snapshotPath) ||
			filepath.Clean(snapshotPath) != snapshotPath) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime package snapshot path is not canonical absolute",
		)
	}
	stat, err := source.Stat()
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("stat runtime package source: %w", err)
	}
	if !stat.Mode().IsRegular() ||
		stat.Size() < 1 ||
		stat.Size() > maxRuntimeReleasePackageBytes {
		return RuntimeReleasePackage{}, fmt.Errorf(
			"runtime package source is not a regular file within [1,%d] bytes",
			maxRuntimeReleasePackageBytes,
		)
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("rewind runtime package source: %w", err)
	}
	packageSnapshot, err := os.CreateTemp(
		scratchDirectory,
		".helmr-runtime-package-input-*",
	)
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("create runtime package input snapshot: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, packageSnapshot.Close())
		returnErr = errors.Join(returnErr, os.Remove(packageSnapshot.Name()))
	}()
	digest := sha256.New()
	sizeBytes, err := copyRuntimeRelease(
		ctx,
		io.MultiWriter(packageSnapshot, digest),
		io.LimitReader(source, maxRuntimeReleasePackageBytes+1),
	)
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("capture runtime package: %w", err)
	}
	if sizeBytes < 1 || sizeBytes > maxRuntimeReleasePackageBytes {
		return RuntimeReleasePackage{}, fmt.Errorf(
			"runtime package size is outside [1,%d]",
			maxRuntimeReleasePackageBytes,
		)
	}
	if err := packageSnapshot.Sync(); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("sync runtime package input snapshot: %w", err)
	}
	if err := packageSnapshot.Chmod(0o400); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("set runtime package input mode: %w", err)
	}
	if _, err := packageSnapshot.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("rewind runtime package input snapshot: %w", err)
	}
	if err := validateRuntimeWorkerPackage(
		packageSnapshot,
		architecture,
		authenticate,
		authenticateToolchain,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}

	extracted, err := os.MkdirTemp(scratchDirectory, ".helmr-runtime-package-files-*")
	if err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("create runtime package extraction: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, os.RemoveAll(extracted))
	}()
	if _, err := packageSnapshot.Seek(0, io.SeekStart); err != nil {
		return RuntimeReleasePackage{}, fmt.Errorf("rewind verified runtime package: %w", err)
	}
	if err := extractRuntimeWorkerPackage(
		packageSnapshot,
		extracted,
		architecture,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	catalogBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, RuntimeReleaseCatalogFile),
		maxRuntimeCatalogBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	bundleBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, RuntimeReleaseBundleFile),
		maxReleaseBundleBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	trustedRootBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, RuntimeReleaseTrustedRootFile),
		maxReleaseTrustedRootBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	catalog, err := authenticate(catalogBytes, bundleBytes, trustedRootBytes)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	toolchainCatalogBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, ToolchainReleaseCatalogFile),
		maxToolchainCatalogBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	toolchainBundleBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, ToolchainReleaseBundleFile),
		maxReleaseBundleBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	toolchainTrustedRootBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, ToolchainReleaseTrustedRootFile),
		maxReleaseTrustedRootBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	if !bytes.Equal(trustedRootBytes, toolchainTrustedRootBytes) {
		return RuntimeReleasePackage{}, errors.New(
			"runtime and standard-toolchain trusted roots do not exact-match",
		)
	}
	toolchainCatalog, err := authenticateToolchain(
		toolchainCatalogBytes,
		toolchainBundleBytes,
		toolchainTrustedRootBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	toolchainObjects, err := toolchainClosureObjects(
		toolchainCatalog,
		architecture,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	if err := validateToolchainObjectDirectory(extracted, toolchainObjects); err != nil {
		return RuntimeReleasePackage{}, err
	}
	corpusBytes, err := readRuntimeReleaseInputFile(
		filepath.Join(extracted, RuntimeReleaseCorpusFile),
		maxRuntimeVerifierCorpusManifestBytes,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	corpus, err := parseRuntimeVerifierCorpusManifest(
		corpusBytes,
		catalog,
		architecture,
	)
	if err != nil {
		return RuntimeReleasePackage{}, err
	}
	if err := verifyExtractedRuntimeCorpus(
		ctx,
		extracted,
		scratchDirectory,
		unitCgroupRoot,
		corpus,
		verify,
		snapshot,
	); err != nil {
		return RuntimeReleasePackage{}, err
	}
	if outputDirectory != "" {
		if err := materializeRuntimeWorkerPackage(
			extracted,
			outputDirectory,
			toolchainObjects,
		); err != nil {
			return RuntimeReleasePackage{}, err
		}
	}
	if snapshotPath != "" {
		if _, err := packageSnapshot.Seek(0, io.SeekStart); err != nil {
			return RuntimeReleasePackage{}, fmt.Errorf(
				"rewind verified runtime package snapshot: %w",
				err,
			)
		}
		if err := materializeRuntimeWorkerSnapshot(
			ctx,
			packageSnapshot,
			sizeBytes,
			snapshotPath,
		); err != nil {
			return RuntimeReleasePackage{}, err
		}
	}
	return RuntimeReleasePackage{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		SizeBytes: sizeBytes,
	}, nil
}

func materializeRuntimeWorkerSnapshot(
	ctx context.Context,
	source io.Reader,
	sizeBytes int64,
	destination string,
) (returnErr error) {
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create runtime package snapshot parent: %w", err)
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("runtime package snapshot already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime package snapshot: %w", err)
	}
	staging, err := os.CreateTemp(parent, "."+filepath.Base(destination)+"-*")
	if err != nil {
		return fmt.Errorf("create runtime package snapshot staging: %w", err)
	}
	stagingPath := staging.Name()
	defer func() {
		if staging != nil {
			returnErr = errors.Join(returnErr, staging.Close())
		}
		if stagingPath != "" {
			returnErr = errors.Join(returnErr, os.Remove(stagingPath))
		}
	}()
	written, err := copyRuntimeRelease(ctx, staging, source)
	if err != nil {
		return fmt.Errorf("write runtime package snapshot: %w", err)
	}
	if written != sizeBytes {
		return fmt.Errorf(
			"runtime package snapshot size = %d, want %d",
			written,
			sizeBytes,
		)
	}
	if err := staging.Sync(); err != nil {
		return fmt.Errorf("sync runtime package snapshot: %w", err)
	}
	if err := staging.Chmod(0o444); err != nil {
		return fmt.Errorf("set runtime package snapshot mode: %w", err)
	}
	if err := staging.Close(); err != nil {
		return fmt.Errorf("close runtime package snapshot: %w", err)
	}
	staging = nil
	if err := os.Link(stagingPath, destination); err != nil {
		return fmt.Errorf("publish runtime package snapshot: %w", err)
	}
	if err := os.Remove(stagingPath); err != nil {
		return fmt.Errorf("remove runtime package snapshot staging: %w", err)
	}
	stagingPath = ""
	return syncRuntimeReleaseDirectory(parent)
}

func extractRuntimeWorkerPackage(
	source io.Reader,
	directory string,
	architecture RuntimeArchitecture,
) error {
	reader := tar.NewReader(source)
	for _, name := range runtimeReleasePackageFiles {
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("read runtime package member %q: %w", name, err)
		}
		if header.Name != name {
			return fmt.Errorf("runtime package member = %q, want %q", header.Name, name)
		}
		if err := validateRuntimeReleaseTarHeader(header); err != nil {
			return err
		}
		if err := os.MkdirAll(
			filepath.Dir(filepath.Join(directory, name)),
			0o755,
		); err != nil {
			return err
		}
		if err := writeRuntimeReleaseReader(
			context.Background(),
			filepath.Join(directory, name),
			header.Size,
			reader,
		); err != nil {
			return err
		}
	}
	raw, err := readRuntimeReleaseInputFile(
		filepath.Join(directory, ToolchainReleaseCatalogFile),
		maxToolchainCatalogBytes,
	)
	if err != nil {
		return err
	}
	catalog, err := ParseToolchainCatalog(raw)
	if err != nil {
		return err
	}
	objects, err := toolchainClosureObjects(catalog, architecture)
	if err != nil {
		return err
	}
	for _, descriptor := range objects {
		name := ToolchainReleaseObjectsDirectory + "/sha256/" +
			strings.TrimPrefix(descriptor.Digest, "sha256:")
		header, err := reader.Next()
		if err != nil {
			return fmt.Errorf("read runtime package member %q: %w", name, err)
		}
		if header.Name != name {
			return fmt.Errorf(
				"runtime package member = %q, want %q",
				header.Name,
				name,
			)
		}
		if err := validateRuntimeReleaseTarHeader(header); err != nil {
			return err
		}
		if header.Size != descriptor.SizeBytes {
			return fmt.Errorf(
				"runtime package member %q size = %d, want %d",
				name,
				header.Size,
				descriptor.SizeBytes,
			)
		}
		path := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeRuntimeReleaseReader(
			context.Background(),
			path,
			header.Size,
			reader,
		); err != nil {
			return err
		}
	}
	if header, err := reader.Next(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("runtime package contains extra member %q", header.Name)
	}
	return syncRuntimeReleaseDirectory(directory)
}

func verifyExtractedRuntimeCorpus(
	ctx context.Context,
	directory,
	scratchDirectory,
	unitCgroupRoot string,
	corpus runtimeVerifierCorpusManifest,
	verify runtimeReleaseVerifier,
	snapshot runtimeReleaseSnapshotter,
) error {
	valid, err := OpenRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseValidFile),
		corpus.Valid.Descriptor.SizeBytes,
	)
	if err != nil {
		return err
	}
	validSnapshot, err := snapshot(
		ctx,
		scratchDirectory,
		corpus.Valid.Descriptor,
		valid,
	)
	validCloseErr := valid.Close()
	if err := errors.Join(err, validCloseErr); err != nil {
		return err
	}
	index, verifyErr := verify(
		ctx,
		unitCgroupRoot,
		"package-valid",
		validSnapshot,
	)
	snapshotCloseErr := validSnapshot.Close()
	if err := errors.Join(verifyErr, snapshotCloseErr); err != nil {
		return fmt.Errorf("verify runtime package valid fixture: %w", err)
	}
	if index != corpus.Valid.ExpectedIndex {
		return fmt.Errorf(
			"runtime package valid index = %#v, want %#v",
			index,
			corpus.Valid.ExpectedIndex,
		)
	}

	invalid, err := OpenRuntimeReleaseFile(
		filepath.Join(directory, RuntimeReleaseInvalidFile),
		corpus.Invalid.Descriptor.SizeBytes,
	)
	if err != nil {
		return err
	}
	invalidSnapshot, err := snapshot(
		ctx,
		scratchDirectory,
		corpus.Invalid.Descriptor,
		invalid,
	)
	invalidCloseErr := invalid.Close()
	if err := errors.Join(err, invalidCloseErr); err != nil {
		return err
	}
	_, verifyErr = verify(
		ctx,
		unitCgroupRoot,
		"package-invalid",
		invalidSnapshot,
	)
	snapshotCloseErr = invalidSnapshot.Close()
	var semanticInvalid *verifierInvalidError
	if errors.As(verifyErr, &semanticInvalid) {
		verifyErr = nil
	} else {
		if verifyErr == nil {
			verifyErr = errors.New("runtime verifier accepted the invalid package fixture")
		} else {
			verifyErr = fmt.Errorf(
				"runtime package invalid fixture did not produce semantic invalidity: %w",
				verifyErr,
			)
		}
	}
	return errors.Join(verifyErr, snapshotCloseErr)
}

func materializeRuntimeWorkerPackage(
	source,
	destination string,
	toolchainObjects []ToolObject,
) (returnErr error) {
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create runtime package output parent: %w", err)
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("runtime package output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runtime package output: %w", err)
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+"-*")
	if err != nil {
		return fmt.Errorf("create runtime package output staging: %w", err)
	}
	defer func() {
		if staging != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(staging))
		}
	}()
	names := append([]string(nil), runtimeReleasePackageFiles...)
	for _, descriptor := range toolchainObjects {
		names = append(
			names,
			ToolchainReleaseObjectsDirectory+"/sha256/"+
				strings.TrimPrefix(descriptor.Digest, "sha256:"),
		)
	}
	for _, name := range names {
		if err := os.MkdirAll(
			filepath.Dir(filepath.Join(staging, name)),
			0o755,
		); err != nil {
			return err
		}
		input, err := OpenRuntimeReleaseFile(
			filepath.Join(source, name),
			runtimeReleaseMaterializeLimit(name, toolchainObjects),
		)
		if err != nil {
			return err
		}
		stat, err := input.Stat()
		if err != nil {
			input.Close()
			return err
		}
		err = writeRuntimeReleaseReader(
			context.Background(),
			filepath.Join(staging, name),
			stat.Size(),
			input,
		)
		closeErr := input.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	if err := syncRuntimeReleaseDirectory(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("publish runtime package output: %w", err)
	}
	staging = ""
	return syncRuntimeReleaseDirectory(parent)
}

func runtimeReleaseMaterializeLimit(
	name string,
	toolchainObjects []ToolObject,
) int64 {
	if name == RuntimeReleaseValidFile {
		return maxRuntimePhysicalBytes
	}
	for _, descriptor := range toolchainObjects {
		if name == ToolchainReleaseObjectsDirectory+"/sha256/"+
			strings.TrimPrefix(descriptor.Digest, "sha256:") {
			return descriptor.SizeBytes
		}
	}
	limit, err := runtimeReleasePackageMemberLimit(name)
	if err != nil {
		return 0
	}
	return limit
}

func readRuntimeReleaseInputFile(path string, maxBytes int64) ([]byte, error) {
	file, err := OpenRuntimeReleaseFile(path, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("open runtime release file %q: %w", path, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read runtime release file %q: %w", path, err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("runtime release file %q exceeds %d bytes", path, maxBytes)
	}
	return raw, nil
}

func validateRuntimeReleaseTarHeader(header *tar.Header) error {
	epoch := time.Unix(0, 0).UTC()
	if header.Typeflag != tar.TypeReg || header.FileInfo().Mode().Type() != 0 {
		return errors.New("member is not regular")
	}
	if header.Mode != runtimeReleasePackageFileMode {
		return fmt.Errorf(
			"mode = %#o, want %#o",
			header.Mode,
			runtimeReleasePackageFileMode,
		)
	}
	if header.Uid != 0 || header.Gid != 0 ||
		header.Uname != "" || header.Gname != "" {
		return errors.New("ownership metadata is not root")
	}
	if !header.ModTime.Equal(epoch) ||
		!header.AccessTime.IsZero() ||
		!header.ChangeTime.IsZero() {
		return errors.New("timestamp metadata is not the Unix epoch")
	}
	if header.Linkname != "" || header.Devmajor != 0 || header.Devminor != 0 ||
		len(header.PAXRecords) != 0 {
		return errors.New("member contains unsupported metadata")
	}
	if header.Format != tar.FormatUSTAR {
		return fmt.Errorf("tar format = %v, want USTAR", header.Format)
	}
	if header.Size < 1 {
		return errors.New("member is empty")
	}
	return nil
}

func runtimeReleasePackageMemberLimit(name string) (int64, error) {
	switch name {
	case RuntimeReleaseCatalogFile:
		return maxRuntimeCatalogBytes, nil
	case RuntimeReleaseBundleFile:
		return maxReleaseBundleBytes, nil
	case RuntimeReleaseTrustedRootFile:
		return maxReleaseTrustedRootBytes, nil
	case ToolchainReleaseCatalogFile:
		return maxToolchainCatalogBytes, nil
	case ToolchainReleaseBundleFile:
		return maxReleaseBundleBytes, nil
	case ToolchainReleaseTrustedRootFile:
		return maxReleaseTrustedRootBytes, nil
	case RuntimeReleaseCorpusFile:
		return maxRuntimeVerifierCorpusManifestBytes, nil
	case RuntimeReleaseInvalidFile:
		return runtimeVerifierCorpusInvalidBytes, nil
	default:
		return 0, fmt.Errorf("runtime package member %q is not small", name)
	}
}

func copyRuntimeRelease(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
) (int64, error) {
	buffer := make([]byte, 128<<10)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			output, err := destination.Write(buffer[:count])
			written += int64(output)
			if err != nil {
				return written, err
			}
			if output != count {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
		if count == 0 {
			return written, io.ErrNoProgress
		}
	}
}

func OpenRuntimeReleaseFile(path string, maxBytes int64) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > maxBytes {
		file.Close()
		return nil, errors.New("runtime release input is not a bounded regular file")
	}
	return file, nil
}
