package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const runtimeArtifactsSchema = "helmr.runtime-artifacts.v0"

type runtimeArtifact struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

type runtimeArtifacts struct {
	Schema            string          `json:"schema"`
	Arch              string          `json:"arch"`
	VMRuntimeContract string          `json:"vm_runtime_contract"`
	Kernel            runtimeArtifact `json:"kernel"`
	Initramfs         runtimeArtifact `json:"initramfs"`
	Rootfs            runtimeArtifact `json:"rootfs"`
}

func loadRuntimeArtifacts(cfg Config) (runtimeArtifacts, error) {
	file, err := os.Open(cfg.RuntimeArtifactsPath)
	if err != nil {
		return runtimeArtifacts{}, fmt.Errorf("open runtime artifacts manifest: %w", err)
	}
	defer file.Close()
	artifacts, err := decodeRuntimeArtifacts(file)
	if err != nil {
		return runtimeArtifacts{}, err
	}
	if err := validateRuntimeArtifactsManifest(cfg, artifacts); err != nil {
		return runtimeArtifacts{}, err
	}
	for _, artifact := range []struct {
		name string
		path string
		item runtimeArtifact
	}{
		{name: "kernel", path: cfg.KernelPath, item: artifacts.Kernel},
		{name: "initramfs", path: cfg.InitramfsPath, item: artifacts.Initramfs},
		{name: "rootfs", path: cfg.RootfsPath, item: artifacts.Rootfs},
	} {
		if err := validateRuntimeArtifact(artifact.name, artifact.path, artifact.item); err != nil {
			return runtimeArtifacts{}, err
		}
	}
	return artifacts, nil
}

func decodeRuntimeArtifacts(source io.Reader) (runtimeArtifacts, error) {
	decoder := json.NewDecoder(source)
	decoder.DisallowUnknownFields()
	var artifacts runtimeArtifacts
	if err := decoder.Decode(&artifacts); err != nil {
		return runtimeArtifacts{}, fmt.Errorf("decode runtime artifacts manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return runtimeArtifacts{}, err
	}
	return artifacts, nil
}

func validateRuntimeArtifactsManifest(cfg Config, artifacts runtimeArtifacts) error {
	if artifacts.Schema != runtimeArtifactsSchema {
		return fmt.Errorf("runtime artifacts schema %q is not supported", artifacts.Schema)
	}
	if artifacts.Arch != runtime.GOARCH {
		return fmt.Errorf("runtime artifacts arch %q does not match worker arch %q", artifacts.Arch, runtime.GOARCH)
	}
	if artifacts.VMRuntimeContract != runtimeid.Contract {
		return fmt.Errorf("runtime artifacts contract %q does not match worker contract %q", artifacts.VMRuntimeContract, runtimeid.Contract)
	}
	for _, artifact := range []struct {
		name string
		path string
		item runtimeArtifact
	}{
		{name: "kernel", path: cfg.KernelPath, item: artifacts.Kernel},
		{name: "initramfs", path: cfg.InitramfsPath, item: artifacts.Initramfs},
		{name: "rootfs", path: cfg.RootfsPath, item: artifacts.Rootfs},
	} {
		if err := validateRuntimeArtifactDeclaration(artifact.name, artifact.path, artifact.item); err != nil {
			return err
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode runtime artifacts manifest: %w", err)
	}
	return errors.New("runtime artifacts manifest contains trailing JSON")
}

func validateRuntimeArtifact(name string, path string, artifact runtimeArtifact) error {
	if err := validateRuntimeArtifactDeclaration(name, path, artifact); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat runtime artifacts %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("runtime artifacts %s is not a regular file", name)
	}
	if info.Size() != artifact.SizeBytes {
		return fmt.Errorf("runtime artifacts %s size %d does not match manifest size %d", name, info.Size(), artifact.SizeBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open runtime artifacts %s: %w", name, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hash runtime artifacts %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close runtime artifacts %s: %w", name, closeErr)
	}
	if written != artifact.SizeBytes {
		return fmt.Errorf("runtime artifacts %s read %d bytes, want %d", name, written, artifact.SizeBytes)
	}
	if digest := sha256sum.DigestHash(hash); digest != artifact.Digest {
		return fmt.Errorf("runtime artifacts %s digest %q does not match manifest digest %q", name, digest, artifact.Digest)
	}
	return nil
}

func validateRuntimeArtifactDeclaration(name string, path string, artifact runtimeArtifact) error {
	if artifact.Path != filepath.Base(path) {
		return fmt.Errorf("runtime artifacts %s path %q does not match %q", name, artifact.Path, filepath.Base(path))
	}
	digest := strings.TrimPrefix(artifact.Digest, "sha256:")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || artifact.Digest != "sha256:"+strings.ToLower(digest) {
		return fmt.Errorf("runtime artifacts %s digest is not canonical sha256", name)
	}
	if artifact.SizeBytes <= 0 {
		return fmt.Errorf("runtime artifacts %s size must be positive", name)
	}
	return nil
}
