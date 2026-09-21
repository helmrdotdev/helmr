package substrate

import (
	"archive/tar"
	"bytes"
	"context"
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
	const expectedSize = int64(256 * 1024 * 1024)

	mkfs := os.Getenv("HELMR_SUBSTRATE_MKFS_EXT4")
	config := os.Getenv("HELMR_SUBSTRATE_MKE2FS_CONFIG")
	e2fsck := os.Getenv("HELMR_SUBSTRATE_E2FSCK")
	debugfs := os.Getenv("HELMR_SUBSTRATE_DEBUGFS")
	if mkfs == "" || config == "" || e2fsck == "" || debugfs == "" {
		t.Skip("exact substrate generator closure is not configured")
	}
	var layer bytes.Buffer
	w := tar.NewWriter(&layer)
	for _, h := range []tar.Header{
		{Name: ".", Typeflag: tar.TypeDir, Mode: 0755, Uid: 1000, Gid: 1000},
		{Name: "home", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "home/agent", Typeflag: tar.TypeDir, Mode: 02700, Uid: 1000, Gid: 1000},
		{Name: "home/agent/payload", Typeflag: tar.TypeReg, Mode: 04750, Uid: 1000, Gid: 1000, Size: 5, ModTime: time.Unix(1234567, 0), Xattrs: map[string]string{"user.helmr": "authored", "security.capability": string([]byte{1, 0, 0, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})}},
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "home/agent/payload"},
		{Name: "symlink", Typeflag: tar.TypeSymlink, Mode: 0777, Uid: 1000, Gid: 1000, Linkname: "home/agent/payload"},
		{Name: "tmp", Typeflag: tar.TypeDir, Mode: 01777},
	} {
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	image := ociTarFromLayers(t, layer.Bytes())

	var firstDigest string
	for iteration := range 2 {
		t.Setenv("LC_ALL", []string{"C", "en_US.UTF-8"}[iteration])
		t.Setenv("TZ", []string{"Pacific/Honolulu", "Asia/Tokyo"}[iteration])
		t.Setenv("SOURCE_DATE_EPOCH", []string{"1", "2000000000"}[iteration])
		t.Setenv("MKE2FS_CONFIG", []string{"/does/not/exist-a", "/does/not/exist-b"}[iteration])
		t.Setenv("E2FSPROGS_FAKE_TIME", []string{"2", "2000000001"}[iteration])
		t.Setenv("MKE2FS_SYNC", []string{"1", "2"}[iteration])
		root := filepath.Join(t.TempDir(), "rootfs")
		_, filesystem, err := oci.UnpackFilesystem(bytes.NewReader(image), root)
		if err != nil {
			t.Fatal(err)
		}

		image := filepath.Join(t.TempDir(), "substrate.ext4")
		if err := createExt4(context.Background(), mkfs, config, filesystem, image, 256*1024*1024, "sha256:fixture"); err != nil {
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
		if sizeBytes != expectedSize {
			t.Fatalf("projection size = %d, want %d", sizeBytes, expectedSize)
		}
		if iteration == 0 {
			firstDigest = digest
			continue
		}
		if digest != firstDigest {
			t.Fatal("identical rootfs content produced different ext4 bytes")
		}
		proveNonrootImageWrite(t, image)
	}
}

func assertProjectedFilesystem(t *testing.T, debugfs, image string) {
	t.Helper()
	for _, tc := range []struct {
		path  string
		parts []string
	}{
		{"<2>", []string{"Type: directory", "User:  1000", "Group:  1000"}},
		{"home/agent", []string{"Mode:  02700", "User:  1000", "Group:  1000"}},
		{"home/agent/payload", []string{"Mode:  04750", "User:  1000", "Group:  1000", "Links: 2", "mtime: 0x0012d687"}},
		{"tmp", []string{"Mode:  01777"}},
	} {
		got := runDebugfs(t, debugfs, image, "stat "+tc.path)
		for _, part := range tc.parts {
			if !strings.Contains(got, part) {
				t.Fatalf("%s missing %q:\n%s", tc.path, part, got)
			}
		}
	}
	if debugfsInode(t, runDebugfs(t, debugfs, image, "stat hardlink")) != debugfsInode(t, runDebugfs(t, debugfs, image, "stat home/agent/payload")) {
		t.Fatal("hardlink inode changed")
	}
	if got := runDebugfs(t, debugfs, image, "ea_list home/agent/payload"); !strings.Contains(got, "user.helmr") || !strings.Contains(got, "authored") || !strings.Contains(got, "security.capability") {
		t.Fatalf("missing authored xattrs: %s", got)
	}
	for name, want := range map[string][]byte{
		"user.helmr":          []byte("authored"),
		"security.capability": {1, 0, 0, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		target := filepath.Join(t.TempDir(), "xattr")
		runDebugfs(t, debugfs, image, "ea_get -f "+target+" home/agent/payload "+name)
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s bytes=%x err=%v want=%x", name, got, err, want)
		}
	}
	if got := runDebugfs(t, debugfs, image, "stat symlink"); !strings.Contains(got, `Fast link dest: "home/agent/payload"`) {
		t.Fatalf("symlink changed: %s", got)
	}
	if got := runDebugfs(t, debugfs, image, "cat home/agent/payload"); got != "hello" {
		t.Fatalf("payload: %q", got)
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
