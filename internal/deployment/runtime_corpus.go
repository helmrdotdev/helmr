package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	RuntimeVerifierCorpusFormatVersion = 0

	runtimeVerifierCorpusManifestPath = "/usr/lib/helmr/runtime-release/verifier-corpus.json"
	runtimeVerifierCorpusValidPath    = "/usr/lib/helmr/runtime-release/verifier-valid.squashfs"
	runtimeVerifierCorpusInvalidPath  = "/usr/lib/helmr/runtime-release/verifier-invalid.squashfs"

	maxRuntimeVerifierCorpusManifestBytes = 16 << 10
	runtimeVerifierCorpusInvalidBytes     = 4096
	runtimeVerifierCorpusScratchOverhead  = 16 << 20
)

type runtimeVerifierCorpusManifest struct {
	FormatVersion int                          `json:"formatVersion"`
	Valid         runtimeVerifierCorpusValid   `json:"valid"`
	Invalid       runtimeVerifierCorpusInvalid `json:"invalid"`
}

type runtimeVerifierCorpusValid struct {
	Descriptor    RuntimeDescriptor `json:"descriptor"`
	ExpectedIndex RuntimeIndex      `json:"expectedIndex"`
}

type runtimeVerifierCorpusInvalid struct {
	Descriptor RuntimeDescriptor `json:"descriptor"`
}

type runtimeVerifierCorpusPaths struct {
	manifest string
	valid    string
	invalid  string
}

type runtimeVerifierCorpus struct {
	manifest *os.File
	valid    *os.File
	invalid  *os.File
	document runtimeVerifierCorpusManifest
}

type runtimeCorpusSnapshotter func(
	context.Context,
	string,
	RuntimeDescriptor,
	io.Reader,
) (*RuntimeArtifactSnapshot, error)

type runtimeCorpusVerifier func(
	context.Context,
	string,
	string,
	*RuntimeArtifactSnapshot,
) (RuntimeIndex, error)

func VerifyRuntimeVerifierCorpus(
	ctx context.Context,
	catalog *RuntimeCatalog,
	architecture RuntimeArchitecture,
	unitCgroupRoot,
	scratchDirectory string,
) (returnErr error) {
	if ctx == nil {
		return errors.New("runtime verifier corpus context is nil")
	}
	corpus, err := openRuntimeVerifierCorpus(
		runtimeVerifierCorpusPaths{
			manifest: runtimeVerifierCorpusManifestPath,
			valid:    runtimeVerifierCorpusValidPath,
			invalid:  runtimeVerifierCorpusInvalidPath,
		},
		catalog,
		architecture,
		0,
	)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, corpus.Close())
	}()

	return verifyRuntimeVerifierCorpus(
		ctx,
		corpus,
		unitCgroupRoot,
		scratchDirectory,
		SnapshotRuntimeArtifact,
		VerifyRuntimeArtifact,
	)
}

func openRuntimeVerifierCorpus(
	paths runtimeVerifierCorpusPaths,
	catalog *RuntimeCatalog,
	architecture RuntimeArchitecture,
	ownerUID uint32,
) (_ *runtimeVerifierCorpus, returnErr error) {
	if err := ValidateRuntimeArchitecture(architecture); err != nil {
		return nil, err
	}
	manifest, err := openRuntimeReleaseFile(
		paths.manifest,
		"runtime verifier corpus manifest",
		maxRuntimeVerifierCorpusManifestBytes,
		ownerUID,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if manifest != nil {
			returnErr = errors.Join(returnErr, manifest.Close())
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(manifest, maxRuntimeVerifierCorpusManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read runtime verifier corpus manifest: %w", err)
	}
	document, err := parseRuntimeVerifierCorpusManifest(raw, catalog, architecture)
	if err != nil {
		return nil, err
	}

	valid, err := openRuntimeReleaseFileExact(
		paths.valid,
		"runtime verifier valid fixture",
		document.Valid.Descriptor.SizeBytes,
		ownerUID,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if valid != nil {
			returnErr = errors.Join(returnErr, valid.Close())
		}
	}()
	invalid, err := openRuntimeReleaseFileExact(
		paths.invalid,
		"runtime verifier invalid fixture",
		document.Invalid.Descriptor.SizeBytes,
		ownerUID,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if invalid != nil {
			returnErr = errors.Join(returnErr, invalid.Close())
		}
	}()

	corpus := &runtimeVerifierCorpus{
		manifest: manifest,
		valid:    valid,
		invalid:  invalid,
		document: document,
	}
	manifest = nil
	valid = nil
	invalid = nil
	return corpus, nil
}

func parseRuntimeVerifierCorpusManifest(
	raw []byte,
	catalog *RuntimeCatalog,
	architecture RuntimeArchitecture,
) (runtimeVerifierCorpusManifest, error) {
	if len(raw) == 0 || len(raw) > maxRuntimeVerifierCorpusManifestBytes {
		return runtimeVerifierCorpusManifest{}, fmt.Errorf(
			"runtime verifier corpus manifest size is outside [1,%d]",
			maxRuntimeVerifierCorpusManifestBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return runtimeVerifierCorpusManifest{}, fmt.Errorf(
			"canonicalize runtime verifier corpus manifest: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return runtimeVerifierCorpusManifest{}, errors.New(
			"runtime verifier corpus manifest is not RFC 8785 canonical JSON",
		)
	}

	var document runtimeVerifierCorpusManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return runtimeVerifierCorpusManifest{}, fmt.Errorf(
			"decode runtime verifier corpus manifest: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "runtime verifier corpus manifest"); err != nil {
		return runtimeVerifierCorpusManifest{}, err
	}
	if err := validateRuntimeVerifierCorpusManifest(document, catalog, architecture); err != nil {
		return runtimeVerifierCorpusManifest{}, err
	}
	complete, err := canonicalRuntimeVerifierCorpusManifest(document)
	if err != nil {
		return runtimeVerifierCorpusManifest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return runtimeVerifierCorpusManifest{}, errors.New(
			"runtime verifier corpus manifest does not match the complete canonical v0 shape",
		)
	}
	return document, nil
}

func validateRuntimeVerifierCorpusManifest(
	document runtimeVerifierCorpusManifest,
	catalog *RuntimeCatalog,
	architecture RuntimeArchitecture,
) error {
	if document.FormatVersion != RuntimeVerifierCorpusFormatVersion {
		return fmt.Errorf(
			"runtime verifier corpus formatVersion = %d, want %d",
			document.FormatVersion,
			RuntimeVerifierCorpusFormatVersion,
		)
	}
	if err := ValidateRuntimeArchitecture(architecture); err != nil {
		return err
	}
	if err := ValidateRuntimeDescriptor(document.Valid.Descriptor); err != nil {
		return fmt.Errorf("runtime verifier valid descriptor: %w", err)
	}
	if document.Valid.Descriptor.SizeBytes > maxRuntimePhysicalBytes {
		return fmt.Errorf(
			"runtime verifier valid fixture exceeds %d bytes",
			maxRuntimePhysicalBytes,
		)
	}
	if err := ValidateRuntimeIndex(document.Valid.ExpectedIndex); err != nil {
		return fmt.Errorf("runtime verifier expected index: %w", err)
	}
	if document.Valid.Descriptor.Architecture != architecture {
		return fmt.Errorf(
			"runtime verifier valid architecture = %q, worker architecture is %q",
			document.Valid.Descriptor.Architecture,
			architecture,
		)
	}
	if document.Valid.ExpectedIndex.Architecture != architecture {
		return fmt.Errorf(
			"runtime verifier expected index architecture = %q, worker architecture is %q",
			document.Valid.ExpectedIndex.Architecture,
			architecture,
		)
	}
	if document.Valid.ExpectedIndex.RuntimeAPIVersion != document.Valid.Descriptor.RuntimeAPIVersion {
		return errors.New("runtime verifier expected index does not match the valid descriptor")
	}
	registered, err := catalog.Resolve(document.Valid.Descriptor.Digest)
	if err != nil {
		return fmt.Errorf("resolve runtime verifier valid descriptor: %w", err)
	}
	if registered != document.Valid.Descriptor {
		return errors.New("runtime verifier valid descriptor is not an exact catalog member")
	}

	if err := ValidateRuntimeDescriptor(document.Invalid.Descriptor); err != nil {
		return fmt.Errorf("runtime verifier invalid descriptor: %w", err)
	}
	if document.Invalid.Descriptor.Architecture != architecture {
		return fmt.Errorf(
			"runtime verifier invalid architecture = %q, worker architecture is %q",
			document.Invalid.Descriptor.Architecture,
			architecture,
		)
	}
	if document.Invalid.Descriptor.SizeBytes != runtimeVerifierCorpusInvalidBytes {
		return fmt.Errorf(
			"runtime verifier invalid fixture size = %d, want %d",
			document.Invalid.Descriptor.SizeBytes,
			runtimeVerifierCorpusInvalidBytes,
		)
	}
	if document.Invalid.Descriptor.Digest == document.Valid.Descriptor.Digest {
		return errors.New("runtime verifier fixtures have the same digest")
	}
	if _, err := catalog.Resolve(document.Invalid.Descriptor.Digest); err == nil {
		return errors.New("runtime verifier invalid descriptor is a catalog member")
	} else if !errors.Is(err, ErrRuntimeNotRegistered) {
		return fmt.Errorf("resolve runtime verifier invalid descriptor: %w", err)
	}
	if document.Invalid.Descriptor.SizeBytes+maxRuntimeVerifierCorpusManifestBytes >
		runtimeVerifierCorpusScratchOverhead {
		return errors.New("runtime verifier corpus metadata exceeds its scratch overhead")
	}
	return nil
}

func canonicalRuntimeVerifierCorpusManifest(
	document runtimeVerifierCorpusManifest,
) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode runtime verifier corpus manifest: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime verifier corpus manifest: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > maxRuntimeVerifierCorpusManifestBytes {
		return nil, fmt.Errorf(
			"runtime verifier corpus manifest size is outside [1,%d]",
			maxRuntimeVerifierCorpusManifestBytes,
		)
	}
	return canonical, nil
}

func verifyRuntimeVerifierCorpus(
	ctx context.Context,
	corpus *runtimeVerifierCorpus,
	unitCgroupRoot,
	scratchDirectory string,
	snapshot runtimeCorpusSnapshotter,
	verify runtimeCorpusVerifier,
) error {
	if corpus == nil || corpus.valid == nil || corpus.invalid == nil {
		return errors.New("runtime verifier corpus is closed")
	}
	if scratchDirectory == "" {
		return errors.New("runtime verifier corpus scratch directory is empty")
	}
	if snapshot == nil || verify == nil {
		return errors.New("runtime verifier corpus operations are nil")
	}
	if err := verifyRuntimeCorpusFixture(
		ctx,
		scratchDirectory,
		corpus.document.Valid.Descriptor,
		corpus.valid,
		snapshot,
		func(snapshot *RuntimeArtifactSnapshot) error {
			index, err := verify(
				ctx,
				unitCgroupRoot,
				"corpus-valid",
				snapshot,
			)
			if err != nil {
				return err
			}
			if index != corpus.document.Valid.ExpectedIndex {
				return fmt.Errorf(
					"runtime verifier valid index = %#v, want %#v",
					index,
					corpus.document.Valid.ExpectedIndex,
				)
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("verify runtime corpus valid fixture: %w", err)
	}
	if err := verifyRuntimeCorpusFixture(
		ctx,
		scratchDirectory,
		corpus.document.Invalid.Descriptor,
		corpus.invalid,
		snapshot,
		func(snapshot *RuntimeArtifactSnapshot) error {
			_, err := verify(
				ctx,
				unitCgroupRoot,
				"corpus-invalid",
				snapshot,
			)
			var invalid *verifierInvalidError
			if !errors.As(err, &invalid) {
				if err == nil {
					return errors.New("runtime verifier accepted the invalid fixture")
				}
				return fmt.Errorf(
					"runtime verifier invalid fixture did not produce semantic invalidity: %w",
					err,
				)
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("verify runtime corpus invalid fixture: %w", err)
	}
	return nil
}

func verifyRuntimeCorpusFixture(
	ctx context.Context,
	scratchDirectory string,
	descriptor RuntimeDescriptor,
	source io.Reader,
	snapshotter runtimeCorpusSnapshotter,
	verify func(*RuntimeArtifactSnapshot) error,
) (returnErr error) {
	snapshot, err := snapshotter(ctx, scratchDirectory, descriptor, source)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, snapshot.Close())
	}()
	return verify(snapshot)
}

func openRuntimeReleaseFile(
	path,
	label string,
	maxBytes int64,
	ownerUID uint32,
) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	if err := validateRuntimeCorpusFile(file, label, 1, maxBytes, ownerUID); err != nil {
		return nil, closeRuntimeCorpusFile(file, err)
	}
	return file, nil
}

func openRuntimeReleaseFileExact(
	path,
	label string,
	sizeBytes int64,
	ownerUID uint32,
) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	if err := validateRuntimeCorpusFile(
		file,
		label,
		sizeBytes,
		sizeBytes,
		ownerUID,
	); err != nil {
		return nil, closeRuntimeCorpusFile(file, err)
	}
	return file, nil
}

func validateRuntimeCorpusFile(
	file *os.File,
	label string,
	minBytes,
	maxBytes int64,
	ownerUID uint32,
) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", label)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s ownership is unavailable", label)
	}
	if stat.Uid != ownerUID {
		return fmt.Errorf("%s owner uid = %d, want %d", label, stat.Uid, ownerUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or other", label)
	}
	if info.Size() < minBytes || info.Size() > maxBytes {
		return fmt.Errorf(
			"%s size is outside [%d,%d]",
			label,
			minBytes,
			maxBytes,
		)
	}
	return nil
}

func closeRuntimeCorpusFile(file *os.File, cause error) error {
	if err := file.Close(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (corpus *runtimeVerifierCorpus) Close() error {
	if corpus == nil {
		return nil
	}
	var closeErr error
	if corpus.manifest != nil {
		closeErr = errors.Join(closeErr, corpus.manifest.Close())
		corpus.manifest = nil
	}
	if corpus.valid != nil {
		closeErr = errors.Join(closeErr, corpus.valid.Close())
		corpus.valid = nil
	}
	if corpus.invalid != nil {
		closeErr = errors.Join(closeErr, corpus.invalid.Close())
		corpus.invalid = nil
	}
	return closeErr
}
