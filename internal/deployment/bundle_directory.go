package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DeploymentBundleDirectory struct {
	Bundle     DeploymentBundle
	BundleJSON []byte
	Digest     string
	Objects    map[string]string
}

func ReadDeploymentBundleDirectory(path string) (DeploymentBundleDirectory, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return DeploymentBundleDirectory{}, fmt.Errorf("read deployment bundle directory: %w", err)
	}
	if err := requireExactDirectoryEntries(entries, []string{"bundle.json", "objects"}, "deployment bundle"); err != nil {
		return DeploymentBundleDirectory{}, err
	}

	bundlePath := filepath.Join(root, "bundle.json")
	bundleInfo, err := os.Lstat(bundlePath)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	if !bundleInfo.Mode().IsRegular() || bundleInfo.Size() < 1 || bundleInfo.Size() > MaxDeploymentBundleBytes {
		return DeploymentBundleDirectory{}, errors.New("deployment bundle bundle.json must be a bounded regular file")
	}
	bundleJSON, err := os.ReadFile(bundlePath)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	bundle, err := ParseDeploymentBundle(bundleJSON)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	digest, err := DeploymentBundleDigest(bundleJSON)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}

	objectsRoot := filepath.Join(root, "objects")
	objectKinds, err := os.ReadDir(objectsRoot)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	if err := requireExactDirectoryEntries(objectKinds, []string{"sha256"}, "deployment bundle objects"); err != nil {
		return DeploymentBundleDirectory{}, err
	}
	shaRoot := filepath.Join(objectsRoot, "sha256")
	objectEntries, err := os.ReadDir(shaRoot)
	if err != nil {
		return DeploymentBundleDirectory{}, err
	}
	expectedNames := make([]string, 0, len(bundle.Objects))
	descriptors := make(map[string]BundleObject, len(bundle.Objects))
	for _, object := range bundle.Objects {
		name := strings.TrimPrefix(object.Digest, "sha256:")
		expectedNames = append(expectedNames, name)
		descriptors[name] = object
	}
	sort.Strings(expectedNames)
	if err := requireExactDirectoryEntries(objectEntries, expectedNames, "deployment bundle object closure"); err != nil {
		return DeploymentBundleDirectory{}, err
	}

	objects := make(map[string]string, len(bundle.Objects))
	for _, name := range expectedNames {
		descriptor := descriptors[name]
		objectPath := filepath.Join(shaRoot, name)
		if err := verifyDeploymentBundleObjectFile(objectPath, descriptor); err != nil {
			return DeploymentBundleDirectory{}, err
		}
		objects[descriptor.Digest] = objectPath
	}
	return DeploymentBundleDirectory{
		Bundle: bundle, BundleJSON: bundleJSON, Digest: digest, Objects: objects,
	}, nil
}

func requireExactDirectoryEntries(entries []os.DirEntry, expected []string, name string) error {
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	sort.Strings(actual)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	if len(actual) != len(want) {
		return fmt.Errorf("%s entries do not match the exact contract", name)
	}
	for index := range actual {
		if actual[index] != want[index] {
			return fmt.Errorf("%s entries do not match the exact contract", name)
		}
	}
	return nil
}

func verifyDeploymentBundleObjectFile(path string, descriptor BundleObject) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("deployment bundle object %s is not a regular file", descriptor.Digest)
	}
	if info.Size() != descriptor.SizeBytes {
		return fmt.Errorf("deployment bundle object %s size does not match descriptor", descriptor.Digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, descriptor.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("hash deployment bundle object %s: %w", descriptor.Digest, err)
	}
	if written != descriptor.SizeBytes {
		return fmt.Errorf("deployment bundle object %s size changed while reading", descriptor.Digest)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != descriptor.Digest {
		return fmt.Errorf("deployment bundle object %s digest does not match bytes", descriptor.Digest)
	}
	return nil
}
