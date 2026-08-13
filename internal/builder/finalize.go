package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/oci"
)

type ObjectSource struct {
	Digest string
	Path   string
}

// BundleInput contains only finalized execution artifacts. Dependency
// installation, package-manager selection, source layout, and builder
// provenance are producer-local concerns and are deliberately absent.
type BundleInput struct {
	Runtime         deployment.RuntimeDescriptor
	Program         deployment.ProgramOutput
	WorkspaceImages []deployment.BundleWorkspaceImage
	Objects         []ObjectSource
}

// FinalizeBundle writes one exact, self-contained deployment bundle directory.
// The destination is published with one directory rename and must not already
// exist; callers never observe a partially written bundle at that path.
func FinalizeBundle(
	ctx context.Context,
	outputDirectory string,
	input BundleInput,
) (_ deployment.DeploymentBundleDirectory, returnErr error) {
	if ctx == nil {
		return deployment.DeploymentBundleDirectory{}, errors.New("bundle finalization context is nil")
	}
	if err := ctx.Err(); err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	if strings.TrimSpace(outputDirectory) == "" {
		return deployment.DeploymentBundleDirectory{}, errors.New("bundle output directory is required")
	}
	if err := deployment.ValidateRuntimeDescriptor(input.Runtime); err != nil {
		return deployment.DeploymentBundleDirectory{}, fmt.Errorf("bundle Runtime: %w", err)
	}
	if err := deployment.ValidateProgramOutput(input.Program); err != nil {
		return deployment.DeploymentBundleDirectory{}, fmt.Errorf("bundle Program: %w", err)
	}

	plan, err := deployment.DeploymentPlanFromProgramIndex(input.Program.Index)
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	workspaceImages := make([]deployment.BundleWorkspaceImage, len(input.WorkspaceImages))
	copy(workspaceImages, input.WorkspaceImages)
	sort.Slice(workspaceImages, func(left, right int) bool {
		return workspaceImages[left].DeclaredID < workspaceImages[right].DeclaredID
	})
	objects := []deployment.BundleObject{{
		Digest: input.Program.Artifact.Digest, SizeBytes: input.Program.Artifact.SizeBytes,
		MediaType: input.Program.Artifact.MediaType,
	}}
	for _, image := range workspaceImages {
		objects = append(objects, deployment.BundleObject{
			Digest: image.Artifact.Digest, SizeBytes: image.Artifact.SizeBytes,
			MediaType: image.Artifact.MediaType,
		})
	}
	deployment.SortDeploymentBundleObjects(objects)
	bundle := deployment.DeploymentBundle{
		Contract: deployment.DeploymentBundleContract,
		Platform: deployment.DeploymentBundlePlatform{
			Architecture: input.Runtime.Architecture,
			OS:           deployment.DeploymentBundleTargetOS,
		},
		Plan: plan,
		Runtime: deployment.DeploymentBundleRuntime{
			Contract: input.Runtime.RuntimeContract,
			Artifact: deployment.BundleObject{
				Digest: input.Runtime.Digest, SizeBytes: input.Runtime.SizeBytes,
				MediaType: input.Runtime.MediaType,
			},
		},
		Program:         input.Program,
		WorkspaceImages: workspaceImages,
		Objects:         objects,
	}
	bundleJSON, err := deployment.CanonicalDeploymentBundle(bundle)
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}

	sources, err := exactObjectSources(input.Objects, objects)
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	output, err := filepath.Abs(outputDirectory)
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	if _, err := os.Lstat(output); err == nil {
		return deployment.DeploymentBundleDirectory{}, errors.New("bundle output directory already exists")
	} else if !os.IsNotExist(err) {
		return deployment.DeploymentBundleDirectory{}, err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(output)+".partial-")
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	defer func() {
		if stage != "" {
			returnErr = errors.Join(returnErr, os.RemoveAll(stage))
		}
	}()
	objectsDirectory := filepath.Join(stage, "objects", "sha256")
	if err := os.MkdirAll(objectsDirectory, 0o755); err != nil {
		return deployment.DeploymentBundleDirectory{}, err
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
			return deployment.DeploymentBundleDirectory{}, err
		}
		if err := verifyFinalObject(ctx, destination, object, input.Program); err != nil {
			return deployment.DeploymentBundleDirectory{}, err
		}
	}
	if err := writeBundleManifest(filepath.Join(stage, "bundle.json"), bundleJSON); err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	staged, err := deployment.ReadDeploymentBundleDirectory(stage)
	if err != nil {
		return deployment.DeploymentBundleDirectory{}, fmt.Errorf("verify finalized bundle: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return deployment.DeploymentBundleDirectory{}, err
	}
	if err := publishBundleDirectory(stage, output); err != nil {
		return deployment.DeploymentBundleDirectory{}, fmt.Errorf("publish finalized bundle: %w", err)
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
	if _, err := deployment.ReadDeploymentBundleDirectory(sourceDirectory); err != nil {
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
	object deployment.BundleObject,
	program deployment.ProgramOutput,
) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open finalized bundle object %s: %w", object.Digest, err)
	}
	defer file.Close()
	switch object.MediaType {
	case deployment.ProgramArtifactMediaType:
		if object.Digest != program.Artifact.Digest {
			return errors.New("finalized Program object does not match Program descriptor")
		}
		if err := deployment.VerifyProgramOutputFile(ctx, file, program); err != nil {
			return fmt.Errorf("verify finalized Program object: %w", err)
		}
	case deployment.WorkspaceImageArtifactMediaType:
		metadata, err := oci.Inspect(file)
		if err != nil {
			return fmt.Errorf("verify finalized workspace image object: %w", err)
		}
		if metadata.ManifestCount != 1 || metadata.Platform == nil ||
			metadata.Platform.OS != deployment.DeploymentBundleTargetOS ||
			metadata.Platform.Architecture != "amd64" {
			return errors.New("finalized workspace image object platform does not match linux/amd64")
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
	expected []deployment.BundleObject,
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
	expected deployment.BundleObject,
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
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
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
