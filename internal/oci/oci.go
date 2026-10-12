package oci

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/klauspost/compress/zstd"
)

const maxJSONBlobBytes = 16 * 1024 * 1024

type Image struct {
	RootfsDir     string
	Config        RuntimeConfig
	Platform      *Platform
	ManifestCount int
}

type RuntimeConfig struct {
	Env        []string
	WorkingDir string
	User       string
	Entrypoint []string
	Cmd        []string
}

type Metadata struct {
	Config        RuntimeConfig
	Platform      *Platform
	ManifestCount int
}

type Index struct {
	Manifests []Descriptor `json:"manifests"`
}

type Manifest struct {
	Config Descriptor   `json:"config"`
	Layers []Descriptor `json:"layers"`
}

type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type configBlob struct {
	Config struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		User       string   `json:"User"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"Config"`
}

func unpack(r io.Reader, destination, scratchRoot string, apply func(io.Reader, string) error) (Image, error) {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return Image{}, fmt.Errorf("create oci destination: %w", err)
	}
	blobsDir, err := os.MkdirTemp(scratchRoot, "helmr-oci-blobs-*")
	if err != nil {
		return Image{}, fmt.Errorf("create oci blob temp dir: %w", err)
	}
	defer os.RemoveAll(blobsDir)
	indexJSON, err := unpackTar(r, blobsDir)
	if err != nil {
		return Image{}, err
	}
	var index Index
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return Image{}, fmt.Errorf("decode oci index: %w", err)
	}
	if len(index.Manifests) == 0 {
		return Image{}, errors.New("oci index has no manifests")
	}
	manifestBytes, err := readBlob(blobsDir, index.Manifests[0].Digest)
	if err != nil {
		return Image{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Image{}, fmt.Errorf("decode oci manifest: %w", err)
	}
	configBytes, err := readBlob(blobsDir, manifest.Config.Digest)
	if err != nil {
		return Image{}, err
	}
	config, err := DecodeConfig(configBytes)
	if err != nil {
		return Image{}, err
	}
	for _, layer := range manifest.Layers {
		if err := applyLayer(blobsDir, layer, destination, apply); err != nil {
			return Image{}, err
		}
	}
	return Image{
		RootfsDir:     destination,
		Config:        config,
		Platform:      index.Manifests[0].Platform,
		ManifestCount: len(index.Manifests),
	}, nil
}

func DecodeConfig(body []byte) (RuntimeConfig, error) {
	var blob configBlob
	if err := json.Unmarshal(body, &blob); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode oci config: %w", err)
	}
	return RuntimeConfig{
		Env:        blob.Config.Env,
		WorkingDir: blob.Config.WorkingDir,
		User:       blob.Config.User,
		Entrypoint: blob.Config.Entrypoint,
		Cmd:        blob.Config.Cmd,
	}, nil
}

func unpackTar(r io.Reader, blobsDir string) ([]byte, error) {
	reader := tar.NewReader(r)
	var indexJSON []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read oci tar: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		switch {
		case name == "index.json":
			body, err := readLimited(reader, maxJSONBlobBytes)
			if err != nil {
				return nil, fmt.Errorf("read oci index: %w", err)
			}
			indexJSON = body
		case strings.HasPrefix(name, "blobs/sha256/"):
			digest := strings.TrimPrefix(name, "blobs/sha256/")
			if err := validateHexDigest(digest); err != nil {
				return nil, err
			}
			path := filepath.Join(blobsDir, digest)
			file, err := os.Create(path)
			if err != nil {
				return nil, fmt.Errorf("create oci blob: %w", err)
			}
			actual, copyErr := copyWithSHA256(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if actual != digest {
				return nil, fmt.Errorf("oci blob %s has sha256 %s", name, actual)
			}
		}
	}
	if len(indexJSON) == 0 {
		return nil, errors.New("oci image tar missing index.json")
	}
	return indexJSON, nil
}

func readBlob(blobsDir, digest string) ([]byte, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(blobsDir, hexDigest)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat oci blob %s: %w", digest, err)
	}
	if info.Size() > maxJSONBlobBytes {
		return nil, fmt.Errorf("oci JSON blob %s exceeds %d bytes", digest, maxJSONBlobBytes)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read oci blob %s: %w", digest, err)
	}
	return body, nil
}

func applyLayer(blobsDir string, layer Descriptor, destination string, apply func(io.Reader, string) error) error {
	hexDigest, err := parseDigest(layer.Digest)
	if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(blobsDir, hexDigest))
	if err != nil {
		return fmt.Errorf("open oci layer %s: %w", layer.Digest, err)
	}
	defer file.Close()
	switch layer.MediaType {
	case "application/vnd.oci.image.layer.v1.tar",
		"application/vnd.docker.image.rootfs.diff.tar":
		return apply(file, destination)
	case "application/vnd.oci.image.layer.v1.tar+gzip",
		"application/vnd.docker.image.rootfs.diff.tar.gzip":
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("open gzip oci layer %s: %w", layer.Digest, err)
		}
		defer gzipReader.Close()
		return apply(gzipReader, destination)
	case "application/vnd.oci.image.layer.v1.tar+zstd":
		zstdReader, err := zstd.NewReader(file)
		if err != nil {
			return fmt.Errorf("open zstd oci layer %s: %w", layer.Digest, err)
		}
		defer zstdReader.Close()
		return apply(zstdReader, destination)
	default:
		return fmt.Errorf("unsupported oci layer media type %q", layer.MediaType)
	}
}

func parseDigest(digest string) (string, error) {
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("unsupported oci digest %q", digest)
	}
	if err := validateHexDigest(hexDigest); err != nil {
		return "", err
	}
	return hexDigest, nil
}

func validateHexDigest(digest string) error {
	if len(digest) != 64 {
		return fmt.Errorf("invalid sha256 digest %q", digest)
	}
	for _, r := range digest {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return fmt.Errorf("invalid sha256 digest %q", digest)
		}
	}
	return nil
}

func ConfinedLayerPath(root, relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." {
		return root, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe oci layer path %q", relative)
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("unsafe oci layer parent %q", current)
		}
	}
	return filepath.Join(root, clean), nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	var buf strings.Builder
	buf.Grow(4096)
	_, err := io.Copy(&buf, io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("blob exceeds %d bytes", max)
	}
	return []byte(buf.String()), nil
}

func copyWithSHA256(w io.Writer, r io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, hash), r); err != nil {
		return "", err
	}
	return sha256sum.HexHash(hash), nil
}

func tarEntryIsRootDir(header *tar.Header) bool {
	if header == nil || header.Typeflag != tar.TypeDir {
		return false
	}
	name := filepath.ToSlash(filepath.Clean(header.Name))
	return name == "." || name == "/"
}
