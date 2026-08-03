package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const platformReleaseManifestFile = "platform-release.json"

type platformReleaseManifest struct {
	FormatVersion  int                `json:"formatVersion"`
	Policy         ArtifactDescriptor `json:"policy"`
	RuntimeHarness ArtifactDescriptor `json:"runtimeHarness"`
	ToolchainBase  ArtifactDescriptor `json:"toolchainBase"`
}

func PublishPlatformRelease(
	ctx context.Context,
	store cas.ImmutableStore,
	directory string,
) error {
	if ctx == nil {
		return errors.New("Platform release publish context is nil")
	}
	if store == nil {
		return errors.New("Platform Artifact store is required")
	}
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("Platform release directory must be canonical and absolute")
	}
	raw, err := os.ReadFile(filepath.Join(directory, platformReleaseManifestFile))
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("Platform release manifest is not canonical JSON")
	}
	var manifest platformReleaseManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if err := ensureEOF(decoder, "Platform release manifest"); err != nil {
		return err
	}
	if manifest.FormatVersion != 0 {
		return errors.New("Platform release manifest format is unsupported")
	}
	if err := validatePlatformTreeInput(manifest.RuntimeHarness, "Runtime harness"); err != nil {
		return err
	}
	if err := validatePlatformTreeInput(manifest.ToolchainBase, "toolchain base"); err != nil {
		return err
	}
	if manifest.Policy.MediaType != BuildPolicyMediaType ||
		!sha256DigestPattern.MatchString(manifest.Policy.Digest) ||
		manifest.Policy.SizeBytes < 1 ||
		manifest.Policy.SizeBytes > maxBuildPolicyBytes {
		return errors.New("Platform release build policy descriptor is invalid")
	}
	policyRaw, err := readReleaseObject(directory, manifest.Policy)
	if err != nil {
		return err
	}
	policy, err := ParseBuildPolicy(policyRaw)
	if err != nil {
		return err
	}
	runtime, toolchain, err := policy.PlatformInputs()
	if err != nil {
		return err
	}
	if runtime.Harness != manifest.RuntimeHarness || toolchain.Base != manifest.ToolchainBase {
		return errors.New("Platform release inputs do not match its build policy")
	}
	for _, descriptor := range []ArtifactDescriptor{
		manifest.RuntimeHarness,
		manifest.ToolchainBase,
		manifest.Policy,
	} {
		file, err := openReleaseObject(directory, descriptor)
		if err != nil {
			return err
		}
		_, publishErr := store.Publish(ctx, releaseCASDescriptor(descriptor), file)
		closeErr := file.Close()
		if publishErr != nil || closeErr != nil {
			return errors.Join(publishErr, closeErr)
		}
	}
	return nil
}

func releaseCASDescriptor(descriptor ArtifactDescriptor) cas.Descriptor {
	return cas.Descriptor{
		Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
	}
}

func readReleaseObject(directory string, descriptor ArtifactDescriptor) ([]byte, error) {
	file, err := openReleaseObject(directory, descriptor)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, descriptor.SizeBytes+1))
	if err != nil || int64(len(raw)) != descriptor.SizeBytes {
		return nil, errors.New("Platform release object size changed")
	}
	return raw, nil
}

func openReleaseObject(directory string, descriptor ArtifactDescriptor) (*os.File, error) {
	name := strings.TrimPrefix(descriptor.Digest, "sha256:")
	if len(name) != 64 || "sha256:"+name != descriptor.Digest {
		return nil, errors.New("Platform release object digest is invalid")
	}
	path := filepath.Join(directory, "objects", "sha256", name)
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !linkInfo.Mode().IsRegular() ||
		linkInfo.Size() != descriptor.SizeBytes {
		return nil, errors.New("Platform release object does not match its descriptor")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil ||
		!info.Mode().IsRegular() ||
		info.Size() != descriptor.SizeBytes {
		_ = file.Close()
		return nil, errors.New("Platform release object does not match its descriptor")
	}
	return file, nil
}
