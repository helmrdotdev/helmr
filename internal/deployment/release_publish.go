package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const platformReleaseManifestFile = "platform-release.json"

type platformReleaseManifest struct {
	FormatVersion int               `json:"formatVersion"`
	Runtime       RuntimeDescriptor `json:"runtime"`
}

// PublishPlatformRelease publishes the Product-owned runtime closure required
// by every deployment bundle. Build tools and package-manager policy are
// producer concerns and are intentionally absent from this release contract.
func PublishPlatformRelease(ctx context.Context, store cas.ImmutableStore, directory string) error {
	if ctx == nil {
		return errors.New("platform release publish context is nil")
	}
	if store == nil {
		return errors.New("platform artifact store is required")
	}
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("platform release directory must be canonical and absolute")
	}
	raw, err := os.ReadFile(filepath.Join(directory, platformReleaseManifestFile))
	if err != nil {
		return err
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.New("platform release manifest is not canonical JSON")
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
		return errors.New("platform release manifest format is unsupported")
	}
	descriptor := manifest.Runtime
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return errors.New("platform release Runtime descriptor is invalid")
	}
	name := strings.TrimPrefix(descriptor.Digest, "sha256:")
	path := filepath.Join(directory, "objects", "sha256", name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.SizeBytes {
		return errors.New("platform release Runtime object does not match its descriptor")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, publishErr := store.Publish(ctx, cas.Descriptor{
		Digest: descriptor.Digest, MediaType: descriptor.MediaType, SizeBytes: descriptor.SizeBytes,
	}, file)
	return errors.Join(publishErr, file.Close())
}
