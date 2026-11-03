// Package hostinstall verifies a release's worker host and runtime bundles
// and installs them on a Linux machine. Machine images and `helmr worker up`
// install through the same path.
package hostinstall

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/helmrdotdev/helmr/internal/release"
)

// Bundles are a release's two bundles on disk and the digests that pin them
// and the manifests they carry.
type Bundles struct {
	HostPath, RuntimePath string
	Host, HostManifest    release.Asset
	Runtime               release.Asset
	RuntimeManifest       release.Asset
}

// FromRelease pins bundles fetched into dir to a verified release index.
func FromRelease(index release.Index, dir string) Bundles {
	return Bundles{
		HostPath:        filepath.Join(dir, release.HostArtifacts),
		RuntimePath:     filepath.Join(dir, release.RuntimeArtifacts),
		Host:            index.Assets[release.HostArtifacts],
		HostManifest:    index.Assets[release.HostArtifactsManifest],
		Runtime:         index.Assets[release.RuntimeArtifacts],
		RuntimeManifest: index.Assets[release.RuntimeArtifactsManifest],
	}
}

const (
	hostManifestName    = "worker-host-artifacts.json"
	runtimeManifestName = "runtime-artifacts.json"
	runtimeContract     = "helmr.vm-runtime.v0"
	// maxMemberBytes bounds one extracted file; runtime root filesystems are
	// the largest members.
	maxMemberBytes = 8 << 30
)

// hostFiles are the host bundle's files in manifest order, with their modes.
var hostFiles = []struct {
	path string
	mode os.FileMode
}{
	{"cpu-template-helper", 0o755}, {"firecracker", 0o755}, {"jailer", 0o755},
	{"mkfs.ext4", 0o755}, {"worker", 0o755}, {"helmr", 0o755}, {"mke2fs.conf", 0o444},
}

var runtimeFiles = []string{"initramfs", "rootfs.squashfs", runtimeManifestName, "vmlinuz"}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Verified holds both bundles' files extracted into a private directory and
// checked against their pins and manifests.
type Verified struct {
	Dir string
}

// Host is the path of a verified host bundle file.
func (v Verified) Host(name string) string { return filepath.Join(v.Dir, "host", name) }

// Runtime is the path of a verified runtime bundle file.
func (v Verified) Runtime(name string) string { return filepath.Join(v.Dir, "runtime", name) }

// Close removes the verified files.
func (v Verified) Close() error { return os.RemoveAll(v.Dir) }

// Verify extracts both bundles into a new private directory under tmp and
// checks every file against the pinned digests and the bundles' manifests.
// It accepts only the bundles' exact regular-file members.
func Verify(bundles Bundles, tmp string) (Verified, error) {
	for _, asset := range []release.Asset{bundles.Host, bundles.HostManifest, bundles.Runtime, bundles.RuntimeManifest} {
		if !digestPattern.MatchString(asset.Digest) {
			return Verified{}, errors.New("invalid artifact digest")
		}
	}
	dir, err := os.MkdirTemp(tmp, "helmr-worker-install-")
	if err != nil {
		return Verified{}, err
	}
	verified := Verified{Dir: dir}
	if err := verify(bundles, verified); err != nil {
		_ = verified.Close()
		return Verified{}, err
	}
	return verified, nil
}

func verify(bundles Bundles, v Verified) error {
	var hostNames []string
	for _, file := range hostFiles {
		hostNames = append(hostNames, file.path)
	}
	hostModes, err := extract(bundles.HostPath, bundles.Host.Digest, filepath.Join(v.Dir, "host"), append(hostNames, hostManifestName))
	if err != nil {
		return fmt.Errorf("worker host bundle: %w", err)
	}
	if err := verifyHostManifest(v, bundles.HostManifest.Digest, hostModes); err != nil {
		return fmt.Errorf("worker host bundle: %w", err)
	}
	if _, err := extract(bundles.RuntimePath, bundles.Runtime.Digest, filepath.Join(v.Dir, "runtime"), runtimeFiles); err != nil {
		return fmt.Errorf("runtime bundle: %w", err)
	}
	if err := verifyRuntimeManifest(v, bundles.RuntimeManifest.Digest); err != nil {
		return fmt.Errorf("runtime bundle: %w", err)
	}
	return nil
}

// extract checks the archive's digest, then writes its members into dir. Each
// member must be a regular file named exactly once from names.
func extract(path, digest, dir string, names []string) (map[string]os.FileMode, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	// Extract from a private copy, so the checked bytes are the extracted ones.
	archive, err := copyChecked(path, filepath.Join(dir, ".archive"), digest)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive)
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	modes := map[string]os.FileMode{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !slices.Contains(names, header.Name) {
			return nil, fmt.Errorf("unexpected archive member %q", header.Name)
		}
		if _, seen := modes[header.Name]; seen {
			return nil, fmt.Errorf("archive member %q repeats", header.Name)
		}
		if header.Size < 0 || header.Size > maxMemberBytes {
			return nil, fmt.Errorf("archive member %q is too large", header.Name)
		}
		out, err := os.OpenFile(filepath.Join(dir, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, io.LimitReader(reader, header.Size))
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		modes[header.Name] = os.FileMode(header.Mode).Perm()
	}
	if len(modes) != len(names) {
		return nil, errors.New("archive is missing members")
	}
	return modes, nil
}

func copyChecked(source, target, digest string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hash), in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		err = errors.New("archive digest does not match its pin")
	}
	if err != nil {
		_ = os.Remove(target)
		return "", err
	}
	return target, nil
}

type fileEntry struct {
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	SizeBytes int64  `json:"size_bytes"`
	Digest    string `json:"digest"`
}

func verifyHostManifest(v Verified, digest string, modes map[string]os.FileMode) error {
	raw, err := readPinned(v.Host(hostManifestName), digest)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 || fields["schema"] == nil || fields["arch"] == nil || fields["files"] == nil {
		return errors.New("manifest must hold exactly schema, arch and files")
	}
	var manifest struct {
		Schema string      `json:"schema"`
		Arch   string      `json:"arch"`
		Files  []fileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if manifest.Schema != "helmr.worker-host-artifacts.v0" || manifest.Arch != "amd64" || len(manifest.Files) != len(hostFiles) {
		return errors.New("manifest is not an amd64 helmr.worker-host-artifacts.v0 manifest of the host files")
	}
	for i, want := range hostFiles {
		entry := manifest.Files[i]
		if entry.Path != want.path || entry.Mode != fmt.Sprintf("%04o", want.mode) || modes[want.path] != want.mode || entry.SizeBytes <= 0 || !digestPattern.MatchString(entry.Digest) {
			return fmt.Errorf("host file %s does not match the manifest", want.path)
		}
		if err := checkFile(v.Host(want.path), entry.Digest, entry.SizeBytes); err != nil {
			return fmt.Errorf("host file %s: %w", want.path, err)
		}
	}
	return nil
}

func verifyRuntimeManifest(v Verified, digest string) error {
	raw, err := readPinned(v.Runtime(runtimeManifestName), digest)
	if err != nil {
		return err
	}
	type object struct {
		Digest    string `json:"digest"`
		SizeBytes int64  `json:"size_bytes"`
	}
	var manifest struct {
		Schema   string          `json:"schema"`
		Arch     string          `json:"arch"`
		Contract json.RawMessage `json:"vm_runtime_contract"`
		Kernel   object          `json:"kernel"`
		Init     object          `json:"initramfs"`
		Rootfs   object          `json:"rootfs"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var contract string
	if manifest.Schema != "helmr.runtime-artifacts.v0" || manifest.Arch != "amd64" || json.Unmarshal(manifest.Contract, &contract) != nil || contract != runtimeContract {
		return fmt.Errorf("manifest is not an amd64 helmr.runtime-artifacts.v0 manifest for %s", runtimeContract)
	}
	for name, entry := range map[string]object{"vmlinuz": manifest.Kernel, "initramfs": manifest.Init, "rootfs.squashfs": manifest.Rootfs} {
		if !digestPattern.MatchString(entry.Digest) {
			return fmt.Errorf("runtime file %s has an invalid digest", name)
		}
		if err := checkFile(v.Runtime(name), entry.Digest, entry.SizeBytes); err != nil {
			return fmt.Errorf("runtime file %s: %w", name, err)
		}
	}
	return nil
}

func readPinned(path, digest string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("manifest digest does not match its pin")
	}
	return bytes.TrimSpace(raw), nil
}

func checkFile(path, digest string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	if n != size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("digest or size does not match the manifest")
	}
	return nil
}
