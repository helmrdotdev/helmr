package builder

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

type ObjectSource struct {
	Digest string
	Path   string
}

// BundleInput contains only finalized execution artifacts. Dependency
// installation, package-manager selection, source layout, and builder
// provenance are producer-local concerns and are deliberately absent.
type BundleInput struct {
	Runtime        artifact.RuntimeDescriptor
	Program        artifact.ProgramOutput
	ComputerImages []bundle.ComputerImage
	Objects        []ObjectSource
}

// FinalizeBundle writes one exact, self-contained deployment bundle directory.
// The destination is published with one directory rename and must not already
// exist; callers never observe a partially written bundle at that path.
func FinalizeBundle(
	ctx context.Context,
	outputDirectory string,
	input BundleInput,
) (_ bundle.Directory, returnErr error) {
	if ctx == nil {
		return bundle.Directory{}, errors.New("bundle finalization context is nil")
	}
	if err := ctx.Err(); err != nil {
		return bundle.Directory{}, err
	}
	if strings.TrimSpace(outputDirectory) == "" {
		return bundle.Directory{}, errors.New("bundle output directory is required")
	}
	if err := artifact.ValidateRuntimeDescriptor(input.Runtime); err != nil {
		return bundle.Directory{}, fmt.Errorf("bundle Runtime: %w", err)
	}
	if err := artifact.ValidateProgramOutput(input.Program); err != nil {
		return bundle.Directory{}, fmt.Errorf("bundle Program: %w", err)
	}

	plan, err := bundle.PlanFromProgramIndex(input.Program.Index)
	if err != nil {
		return bundle.Directory{}, err
	}
	computerImages := make([]bundle.ComputerImage, len(input.ComputerImages))
	copy(computerImages, input.ComputerImages)
	sort.Slice(computerImages, func(left, right int) bool {
		return computerImages[left].DeclaredID < computerImages[right].DeclaredID
	})
	objects, err := referencedBundleObjects(input.Program.Artifact, computerImages)
	if err != nil {
		return bundle.Directory{}, err
	}
	manifest := bundle.Manifest{
		Contract: bundle.Contract,
		Platform: bundle.Platform{
			Architecture: input.Runtime.Architecture,
			OS:           bundle.TargetOS,
		},
		Plan: plan,
		Runtime: bundle.Runtime{
			Contract: input.Runtime.RuntimeContract,
			Artifact: bundle.Object{
				Digest: input.Runtime.Digest, SizeBytes: input.Runtime.SizeBytes,
				MediaType: input.Runtime.MediaType,
			},
		},
		Program:        input.Program,
		ComputerImages: computerImages,
		Objects:        objects,
	}
	bundleJSON, err := bundle.Canonical(manifest)
	if err != nil {
		return bundle.Directory{}, err
	}

	sources, err := exactObjectSources(input.Objects, objects)
	if err != nil {
		return bundle.Directory{}, err
	}
	output, err := filepath.Abs(outputDirectory)
	if err != nil {
		return bundle.Directory{}, err
	}
	if _, err := os.Lstat(output); err == nil {
		return bundle.Directory{}, errors.New("bundle output directory already exists")
	} else if !os.IsNotExist(err) {
		return bundle.Directory{}, err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return bundle.Directory{}, err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(output)+".partial-")
	if err != nil {
		return bundle.Directory{}, err
	}
	defer func() {
		if stage != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(stage))
		}
	}()
	objectsDirectory := filepath.Join(stage, "objects", "sha256")
	if err := os.MkdirAll(objectsDirectory, 0o755); err != nil {
		return bundle.Directory{}, err
	}
	for _, object := range objects {
		destination := filepath.Join(
			objectsDirectory,
			strings.TrimPrefix(object.Digest, "sha256:"),
		)
		if err := copyExactObject(
			ctx,
			sources[object.Digest],
			destination,
			object,
		); err != nil {
			return bundle.Directory{}, err
		}
		if err := verifyFinalObject(ctx, destination, object, input.Program); err != nil {
			return bundle.Directory{}, err
		}
	}
	if err := writeBundleManifest(filepath.Join(stage, "bundle.json"), bundleJSON); err != nil {
		return bundle.Directory{}, err
	}
	staged, err := bundle.ReadDirectory(stage)
	if err != nil {
		return bundle.Directory{}, fmt.Errorf("verify finalized bundle: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return bundle.Directory{}, err
	}
	if err := publishBundleDirectory(stage, output); err != nil {
		return bundle.Directory{}, fmt.Errorf("publish finalized bundle: %w", err)
	}
	stage = ""
	for digest := range staged.Objects {
		staged.Objects[digest] = filepath.Join(
			output,
			"objects",
			"sha256",
			strings.TrimPrefix(digest, "sha256:"),
		)
	}
	return staged, nil
}

func referencedBundleObjects(
	program artifact.ProgramDescriptor,
	computerImages []bundle.ComputerImage,
) ([]bundle.Object, error) {
	objectsByDigest := make(map[string]bundle.Object, 1+len(computerImages))
	programObject := bundle.Object(program)
	objectsByDigest[programObject.Digest] = programObject
	for _, image := range computerImages {
		object := bundle.Object{
			Digest: image.Artifact.Digest, SizeBytes: image.Artifact.SizeBytes,
			MediaType: image.Artifact.MediaType,
		}
		if existing, exists := objectsByDigest[object.Digest]; exists {
			if existing != object {
				return nil, fmt.Errorf(
					"bundle object digest %q has conflicting reference metadata",
					object.Digest,
				)
			}
			continue
		}
		objectsByDigest[object.Digest] = object
	}
	objects := make([]bundle.Object, 0, len(objectsByDigest))
	for _, object := range objectsByDigest {
		objects = append(objects, object)
	}
	bundle.SortObjects(objects)
	return objects, nil
}

// PublishBundleDirectory validates and atomically installs a complete BuildKit
// local output at its final user-visible path. The destination must not exist.
func PublishBundleDirectory(sourceDirectory, outputDirectory string) error {
	for name, value := range map[string]string{
		"bundle source directory": sourceDirectory,
		"bundle output directory": outputDirectory,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	if _, err := bundle.ReadDirectory(sourceDirectory); err != nil {
		return fmt.Errorf("validate BuildKit bundle output: %w", err)
	}
	if err := publishBundleDirectory(sourceDirectory, outputDirectory); err != nil {
		return fmt.Errorf("publish BuildKit bundle output: %w", err)
	}
	return nil
}

func verifyFinalObject(
	ctx context.Context,
	path string,
	object bundle.Object,
	program artifact.ProgramOutput,
) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open finalized bundle object %s: %w", object.Digest, err)
	}
	defer file.Close()
	switch object.MediaType {
	case artifact.ProgramArtifactMediaType:
		if object.Digest != program.Artifact.Digest {
			return errors.New("finalized Program object does not match Program descriptor")
		}
		if err := verify.ProgramOutputFile(ctx, file, program); err != nil {
			return fmt.Errorf("verify finalized Program object: %w", err)
		}
	case bundle.ComputerImageMediaType:
		artifact := disk.SeedArtifact{Object: cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}, LogicalBytes: disk.SeedCapacity}
		if err := disk.VerifySeed(ctx, file, artifact, disk.SeedCapacity); err != nil {
			return fmt.Errorf("verify finalized computer image object: %w", err)
		}

	default:
		return fmt.Errorf("finalized bundle object mediaType %q is unsupported", object.MediaType)
	}
	return nil
}

func writeBundleManifest(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	open := true
	defer func() {
		if open {
			_ = file.Close()
		}
	}()
	written, err := file.Write(body)
	if err != nil {
		return fmt.Errorf("write bundle manifest: %w", err)
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync bundle manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close bundle manifest: %w", err)
	}
	open = false
	return nil
}

func exactObjectSources(
	sources []ObjectSource,
	expected []bundle.Object,
) (map[string]string, error) {
	byDigest := make(map[string]string, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.Path) == "" {
			return nil, errors.New("bundle object source path is required")
		}
		if _, duplicate := byDigest[source.Digest]; duplicate {
			return nil, fmt.Errorf("bundle object source %q is duplicated", source.Digest)
		}
		byDigest[source.Digest] = source.Path
	}
	if len(byDigest) != len(expected) {
		return nil, errors.New("bundle object sources do not match the referenced closure")
	}
	for _, object := range expected {
		if _, exists := byDigest[object.Digest]; !exists {
			return nil, fmt.Errorf("bundle object source %q is missing", object.Digest)
		}
	}
	return byDigest, nil
}

func copyExactObject(
	ctx context.Context,
	sourcePath string,
	destinationPath string,
	expected bundle.Object,
) error {
	before, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect bundle object %s: %w", expected.Digest, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("bundle object source %s is not a regular file", expected.Digest)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open bundle object %s: %w", expected.Digest, err)
	}
	defer source.Close()
	after, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened bundle object %s: %w", expected.Digest, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return fmt.Errorf("bundle object source %s changed before opening", expected.Digest)
	}
	if after.Size() != expected.SizeBytes {
		return fmt.Errorf("bundle object source %s size does not match descriptor", expected.Digest)
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	openDestination := true
	defer func() {
		if openDestination {
			_ = destination.Close()
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(
		io.MultiWriter(destination, hash),
		io.LimitReader(&contextReader{ctx: ctx, reader: source}, expected.SizeBytes+1),
	)
	if err != nil {
		return fmt.Errorf("copy bundle object %s: %w", expected.Digest, err)
	}
	if written != expected.SizeBytes {
		return fmt.Errorf("bundle object source %s size changed while reading", expected.Digest)
	}
	actual := sha256sum.FormatDigest(hash.Sum(nil))
	if actual != expected.Digest {
		return fmt.Errorf("bundle object source %s digest does not match descriptor", expected.Digest)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("sync bundle object %s: %w", expected.Digest, err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close bundle object %s: %w", expected.Digest, err)
	}
	openDestination = false
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}
