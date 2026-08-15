package substrate

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCanonicalRootfsArchiveNormalizesMetadataAndPreservesLinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "dir", "file")
	if err := os.WriteFile(file, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir/file", filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_800_000_000, 0)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeCanonicalRootfsArchive(root, &archive); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	var names []string
	hardlinkSeen := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Uid != 0 || header.Gid != 0 || !header.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("entry %q retains host metadata: uid=%d gid=%d mtime=%s", header.Name, header.Uid, header.Gid, header.ModTime)
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "SCHILY.xattr.") {
				t.Fatalf("entry %q retains xattr %q", header.Name, key)
			}
		}
		if header.Typeflag == tar.TypeLink {
			hardlinkSeen = true
		}
	}
	if want := []string{"dir/", "dir/file", "hardlink", "symlink"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("archive entries = %v, want %v", names, want)
	}
	if !hardlinkSeen {
		t.Fatal("canonical archive did not preserve hardlink identity")
	}
}
