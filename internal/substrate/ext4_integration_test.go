package substrate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/oci"
)

func TestDeterministicExt4Projection(t *testing.T) {
	const expectedDigest = "sha256:0ddc710bea6d99c47a19ad9c18fb7a2b084cc1562ae1aa239f18ea6caf2c2ee2"
	const expectedSize = int64(256 * 1024 * 1024)

	mkfs := os.Getenv("HELMR_SUBSTRATE_MKFS_EXT4")
	config := os.Getenv("HELMR_SUBSTRATE_MKE2FS_CONFIG")
	e2fsck := os.Getenv("HELMR_SUBSTRATE_E2FSCK")
	debugfs := os.Getenv("HELMR_SUBSTRATE_DEBUGFS")
	if mkfs == "" || config == "" || e2fsck == "" || debugfs == "" {
		t.Skip("exact substrate generator closure is not configured")
	}
	baseLayer := projectionLayer(t, "removed.txt")
	whiteoutLayer := projectionLayer(t, ".wh.removed.txt")
	image := ociTarFromLayers(t, baseLayer, whiteoutLayer)

	var firstDigest string
	for iteration := range 2 {
		t.Setenv("LC_ALL", []string{"C", "en_US.UTF-8"}[iteration])
		t.Setenv("TZ", []string{"Pacific/Honolulu", "Asia/Tokyo"}[iteration])
		t.Setenv("SOURCE_DATE_EPOCH", []string{"1", "2000000000"}[iteration])
		t.Setenv("MKE2FS_CONFIG", []string{"/does/not/exist-a", "/does/not/exist-b"}[iteration])
		t.Setenv("E2FSPROGS_FAKE_TIME", []string{"2", "2000000001"}[iteration])
		t.Setenv("MKE2FS_SYNC", []string{"1", "2"}[iteration])
		root := filepath.Join(t.TempDir(), "rootfs")
		if _, err := oci.Unpack(bytes.NewReader(image), root); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, "removed.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("whiteout did not remove base entry: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, "nested", "日本語"), 0o700); err != nil {
			t.Fatal(err)
		}
		regular := filepath.Join(root, "nested", "日本語", "payload.txt")
		if err := os.WriteFile(regular, []byte("deterministic substrate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(regular, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(regular, filepath.Join(root, "payload-hardlink")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("nested/日本語/payload.txt", filepath.Join(root, "payload-symlink")); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(iteration*1000), 0)
		if err := os.Chtimes(regular, stamp, stamp); err != nil {
			t.Fatal(err)
		}

		image := filepath.Join(t.TempDir(), "substrate.ext4")
		if err := createExt4(context.Background(), mkfs, config, root, image, 256*1024*1024, "sha256:fixture"); err != nil {
			t.Fatal(err)
		}
		check := exec.Command(e2fsck, "-fn", image)
		if output, err := check.CombinedOutput(); err != nil {
			t.Fatalf("validate substrate filesystem: %v: %s", err, output)
		}
		assertProjectedFilesystem(t, debugfs, image)
		digest, sizeBytes, err := fileDigest(image)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("projection digest: %s", digest)
		if digest != expectedDigest || sizeBytes != expectedSize {
			t.Fatalf("projection identity = (%s, %d), want (%s, %d)", digest, sizeBytes, expectedDigest, expectedSize)
		}
		if iteration == 0 {
			firstDigest = digest
			continue
		}
		if digest != firstDigest {
			t.Fatal("identical rootfs content produced different ext4 bytes")
		}
	}
}

func assertProjectedFilesystem(t *testing.T, debugfs, image string) {
	t.Helper()
	rootStat := runDebugfs(t, debugfs, image, "stat <2>")
	if !strings.Contains(rootStat, "Type: directory") || !strings.Contains(rootStat, "Mode:  0755") || !strings.Contains(rootStat, "User:     0") || !strings.Contains(rootStat, "Group:     0") {
		t.Fatalf("root metadata was not preserved:\n%s", rootStat)
	}
	regularStat := runDebugfs(t, debugfs, image, "stat nested/日本語/payload.txt")
	if !strings.Contains(regularStat, "Type: regular") || !strings.Contains(regularStat, "Mode:  0640") || !strings.Contains(regularStat, "User:     0") || !strings.Contains(regularStat, "Group:     0") || !strings.Contains(regularStat, "Links: 2") {
		t.Fatalf("regular file metadata was not preserved:\n%s", regularStat)
	}
	hardlinkStat := runDebugfs(t, debugfs, image, "stat payload-hardlink")
	if debugfsInode(t, hardlinkStat) != debugfsInode(t, regularStat) {
		t.Fatalf("hardlink identity was not preserved:\nregular:\n%s\nhardlink:\n%s", regularStat, hardlinkStat)
	}
	symlinkStat := runDebugfs(t, debugfs, image, "stat payload-symlink")
	if !strings.Contains(symlinkStat, "Type: symlink") || !strings.Contains(symlinkStat, `Fast link dest: "nested/日本語/payload.txt"`) {
		t.Fatalf("symlink identity was not preserved:\n%s", symlinkStat)
	}
	if body := runDebugfs(t, debugfs, image, "cat nested/日本語/payload.txt"); body != "deterministic substrate\n" {
		t.Fatalf("projected payload = %q", body)
	}
	missing := runDebugfs(t, debugfs, image, "stat removed.txt")
	if !strings.Contains(missing, "File not found") {
		t.Fatalf("whiteouted entry exists in projected filesystem:\n%s", missing)
	}
}

func runDebugfs(t *testing.T, debugfs, image, command string) string {
	t.Helper()
	cmd := exec.Command(debugfs, "-R", command, image)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TZ=UTC"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect projected filesystem with %q: %v: %s", command, err, output)
	}
	result := string(output)
	if strings.HasPrefix(result, "debugfs ") {
		_, result, _ = strings.Cut(result, "\n")
	}
	return result
}

func debugfsInode(t *testing.T, output string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^Inode:\s+([0-9]+)`).FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("debugfs stat omitted inode:\n%s", output)
	}
	return match[1]
}

func projectionLayer(t *testing.T, name string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	writeTarFile(t, writer, name, nil)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
