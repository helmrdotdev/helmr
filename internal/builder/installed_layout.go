package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	installedLayoutVersion   = "1.0.0"
	installedManifestMedia   = "application/vnd.oci.image.manifest.v1+json"
	maxInstalledLayoutJSON   = 16 << 20
	maxInstalledManifestSize = 16 << 20
)

type installedLayoutDocument struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

// InstalledLayoutContext validates the producer-private OCI layout and returns
// the exact digest-bound BuildKit named-context reference used by every
// downstream solve.
func InstalledLayoutContext(directory string) (string, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("installed OCI layout path is invalid")
	}
	if strings.ContainsAny(directory, "@?#\x00\r\n") {
		return "", errors.New("installed OCI layout path cannot be represented as a BuildKit context")
	}
	var layout installedLayoutDocument
	if err := readBoundedJSON(filepath.Join(directory, "oci-layout"), &layout); err != nil {
		return "", fmt.Errorf("read installed OCI layout version: %w", err)
	}
	if layout.ImageLayoutVersion != installedLayoutVersion {
		return "", errors.New("installed OCI layout version is unsupported")
	}
	var index oci.Index
	if err := readBoundedJSON(filepath.Join(directory, "index.json"), &index); err != nil {
		return "", fmt.Errorf("read installed OCI layout index: %w", err)
	}
	if len(index.Manifests) != 1 {
		return "", errors.New("installed OCI layout must contain exactly one manifest")
	}
	descriptor := index.Manifests[0]
	if descriptor.MediaType != installedManifestMedia ||
		!sha256sum.ValidDigest(descriptor.Digest) ||
		descriptor.Size < 1 || descriptor.Size > maxInstalledManifestSize ||
		descriptor.Platform == nil ||
		descriptor.Platform.OS != "linux" || descriptor.Platform.Architecture != "amd64" {
		return "", errors.New("installed OCI layout manifest is invalid")
	}
	manifestPath := filepath.Join(directory, "blobs", "sha256", strings.TrimPrefix(descriptor.Digest, sha256sum.Prefix))
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() != descriptor.Size {
		return "", errors.New("installed OCI manifest is not a bounded regular file")
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read installed OCI manifest: %w", err)
	}
	if int64(len(manifest)) != descriptor.Size || sha256sum.DigestBytes(manifest) != descriptor.Digest {
		return "", errors.New("installed OCI manifest does not match its descriptor")
	}
	return "oci-layout://" + filepath.ToSlash(directory) + "@" + descriptor.Digest, nil
}

func readBoundedJSON(path string, target any) error {
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() < 1 || pathInfo.Size() > maxInstalledLayoutJSON {
		return errors.New("document is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, info) {
		_ = file.Close()
		return errors.New("document is not a bounded regular file")
	}
	raw, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("document contains trailing data")
	}
	return nil
}
