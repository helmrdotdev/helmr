package oci

import (
	"archive/tar"
	"bytes"
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
	RootfsDir string
	Config    RuntimeConfig
}

type RuntimeConfig struct {
	Env        []string
	WorkingDir string
	User       string
	Entrypoint []string
	Cmd        []string
}

type Index struct {
	Manifests []Descriptor `json:"manifests"`
}

type Manifest struct {
	Config Descriptor   `json:"Config"`
	Layers []Descriptor `json:"layers"`
}

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
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

func Unpack(r io.Reader, destination string) (Image, error) {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return Image{}, fmt.Errorf("create oci destination: %w", err)
	}
	blobsDir, err := os.MkdirTemp("", "helmr-oci-blobs-*")
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
		if err := applyLayer(blobsDir, layer, destination); err != nil {
			return Image{}, err
		}
	}
	return Image{RootfsDir: destination, Config: config}, nil
}

func ReadConfig(r io.Reader) (RuntimeConfig, error) {
	indexJSON, blobs, err := readConfigBlobs(r)
	if err != nil {
		return RuntimeConfig{}, err
	}
	var index Index
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode oci index: %w", err)
	}
	if len(index.Manifests) == 0 {
		return RuntimeConfig{}, errors.New("oci index has no manifests")
	}
	manifestBytes, err := readConfigBlob(blobs, index.Manifests[0].Digest)
	if err != nil {
		return RuntimeConfig{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode oci manifest: %w", err)
	}
	configBytes, err := readConfigBlob(blobs, manifest.Config.Digest)
	if err != nil {
		return RuntimeConfig{}, err
	}
	return DecodeConfig(configBytes)
}

func DecodeConfig(body []byte) (RuntimeConfig, error) {
	var blob configBlob
	if err := json.Unmarshal(body, &blob); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode oci Config: %w", err)
	}
	return RuntimeConfig{
		Env:        blob.Config.Env,
		WorkingDir: blob.Config.WorkingDir,
		User:       blob.Config.User,
		Entrypoint: blob.Config.Entrypoint,
		Cmd:        blob.Config.Cmd,
	}, nil
}

func ApplyLayerTar(r io.Reader, destination string) error {
	reader := tar.NewReader(r)
	currentLayerEntries := map[string]bool{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read oci layer: %w", err)
		}
		if tarEntryIsRootDir(header) {
			continue
		}
		relative := filepath.ToSlash(filepath.Clean(header.Name))
		if relative == "." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
			return fmt.Errorf("unsafe oci layer path %q", header.Name)
		}
		if applied, err := applyWhiteout(destination, relative, currentLayerEntries); err != nil {
			return err
		} else if applied {
			continue
		}
		target, err := confinedLayerPath(destination, relative)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirLayerDir(destination, relative, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
			currentLayerEntries[relative] = true
		case tar.TypeReg:
			if err := ensureLayerParentDir(destination, relative); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			currentLayerEntries[relative] = true
		case tar.TypeSymlink:
			if err := validateLayerSymlinkTarget(relative, header.Linkname); err != nil {
				return err
			}
			if err := ensureLayerParentDir(destination, relative); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
			currentLayerEntries[relative] = true
		case tar.TypeLink:
			if filepath.IsAbs(header.Linkname) || strings.HasPrefix(filepath.Clean(header.Linkname), "..") {
				return fmt.Errorf("unsafe hardlink target %q for %q", header.Linkname, header.Name)
			}
			linkTarget, err := confinedLayerPath(destination, header.Linkname)
			if err != nil {
				return err
			}
			if err := ensureLayerParentDir(destination, relative); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
			currentLayerEntries[relative] = true
		default:
			return fmt.Errorf("unsupported oci layer entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

func readConfigBlobs(r io.Reader) ([]byte, map[string][]byte, error) {
	reader := tar.NewReader(r)
	var indexJSON []byte
	blobs := map[string][]byte{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read oci tar: %w", err)
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		switch {
		case name == "index.json":
			body, err := readLimited(reader, maxJSONBlobBytes)
			if err != nil {
				return nil, nil, fmt.Errorf("read oci index: %w", err)
			}
			indexJSON = body
		case strings.HasPrefix(name, "blobs/sha256/"):
			digest := strings.TrimPrefix(name, "blobs/sha256/")
			if err := validateHexDigest(digest); err != nil {
				return nil, nil, err
			}
			hash := sha256.New()
			var buf bytes.Buffer
			writer := io.Writer(hash)
			if header.Size >= 0 && header.Size <= maxJSONBlobBytes {
				writer = io.MultiWriter(hash, &buf)
			}
			if _, err := io.Copy(writer, reader); err != nil {
				return nil, nil, fmt.Errorf("read oci blob %s: %w", name, err)
			}
			actual := sha256sum.HexHash(hash)
			if actual != digest {
				return nil, nil, fmt.Errorf("oci blob %s has sha256 %s", name, actual)
			}
			if buf.Len() > 0 {
				blobs[digest] = append([]byte(nil), buf.Bytes()...)
			}
		}
	}
	if len(indexJSON) == 0 {
		return nil, nil, errors.New("oci image tar missing index.json")
	}
	return indexJSON, blobs, nil
}

func readConfigBlob(blobs map[string][]byte, digest string) ([]byte, error) {
	hexDigest, err := parseDigest(digest)
	if err != nil {
		return nil, err
	}
	body, ok := blobs[hexDigest]
	if !ok {
		return nil, fmt.Errorf("oci JSON blob %s is missing or exceeds %d bytes", digest, maxJSONBlobBytes)
	}
	return body, nil
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

func applyLayer(blobsDir string, layer Descriptor, destination string) error {
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
		return ApplyLayerTar(file, destination)
	case "application/vnd.oci.image.layer.v1.tar+gzip",
		"application/vnd.docker.image.rootfs.diff.tar.gzip":
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("open gzip oci layer %s: %w", layer.Digest, err)
		}
		defer gzipReader.Close()
		return ApplyLayerTar(gzipReader, destination)
	case "application/vnd.oci.image.layer.v1.tar+zstd":
		zstdReader, err := zstd.NewReader(file)
		if err != nil {
			return fmt.Errorf("open zstd oci layer %s: %w", layer.Digest, err)
		}
		defer zstdReader.Close()
		return ApplyLayerTar(zstdReader, destination)
	default:
		return fmt.Errorf("unsupported oci layer media type %q", layer.MediaType)
	}
}

func applyWhiteout(destination, relative string, currentLayerEntries map[string]bool) (bool, error) {
	base := filepath.Base(relative)
	if base == ".wh..wh..opq" {
		parent := filepath.Dir(relative)
		if parent == "." {
			parent = ""
		}
		target, err := confinedLayerPath(destination, parent)
		if err != nil {
			return false, err
		}
		entries, err := readLayerDir(target)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			entryRelative := filepath.ToSlash(filepath.Join(parent, entry.Name()))
			if currentLayerEntries[entryRelative] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	name, ok := strings.CutPrefix(base, ".wh.")
	if !ok {
		return false, nil
	}
	parent := filepath.Dir(relative)
	if parent == "." {
		parent = ""
	}
	target, err := confinedLayerPath(destination, filepath.Join(parent, name))
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(target); err != nil {
		return false, err
	}
	return true, nil
}

func ensureLayerParentDir(destination, relative string) error {
	parent := filepath.Dir(relative)
	if parent == "." {
		return nil
	}
	return mkdirLayerDir(destination, parent, 0o755)
}

func mkdirLayerDir(destination, relative string, mode os.FileMode) error {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." {
		return nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe oci layer path %q", relative)
	}
	current := destination
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("unsafe oci layer directory %q", current)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		perm := os.FileMode(0o755)
		if i == len(parts)-1 {
			perm = mode
		}
		if err := os.Mkdir(current, perm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe oci layer directory %q", current)
		}
	}
	return nil
}

func readLayerDir(path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("unsafe oci layer directory %q", path)
	}
	return os.ReadDir(path)
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

func confinedLayerPath(root, relative string) (string, error) {
	return ConfinedLayerPath(root, relative)
}

func validateLayerSymlinkTarget(linkPath, target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("unsafe symlink target %q for %q", target, linkPath)
	}
	if filepath.IsAbs(target) {
		return nil
	}
	return validateSymlinkTarget(linkPath, target)
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

func validateSymlinkTarget(linkPath, target string) error {
	if target == "" || filepath.IsAbs(target) {
		return fmt.Errorf("unsafe symlink target %q for %q", target, linkPath)
	}
	depth := len(strings.Split(filepath.Dir(linkPath), string(filepath.Separator)))
	if filepath.Dir(linkPath) == "." {
		depth = 0
	}
	for part := range strings.SplitSeq(filepath.Clean(target), string(filepath.Separator)) {
		switch part {
		case ".", "":
		case "..":
			if depth == 0 {
				return fmt.Errorf("unsafe symlink target %q for %q", target, linkPath)
			}
			depth--
		default:
			depth++
		}
	}
	return nil
}

func tarEntryIsRootDir(header *tar.Header) bool {
	if header == nil || header.Typeflag != tar.TypeDir {
		return false
	}
	name := filepath.ToSlash(filepath.Clean(header.Name))
	return name == "." || name == "/"
}
