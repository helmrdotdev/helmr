package oci

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func metadataLayer(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range headers {
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func metadataFilesystem(t *testing.T, layers ...[]byte) *Filesystem {
	t.Helper()
	input := make([]ociTestLayer, len(layers))
	for i, l := range layers {
		input[i] = ociTestLayer{mediaType: "application/vnd.oci.image.layer.v1.tar", body: l}
	}
	_, fs, err := UnpackFilesystem(bytes.NewReader(ociTar(t, input, []byte(`{"Config":{}}`))), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}
func filesystemHeaders(t *testing.T, fs *Filesystem) map[string]tar.Header {
	t.Helper()
	var b bytes.Buffer
	if err := fs.WriteArchive(&b); err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(&b)
	out := map[string]tar.Header{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out[path.Clean(h.Name)] = *h
	}
	return out
}
func TestFilesystemPreservesAuthoredMetadataWithoutHostPrivileges(t *testing.T) {
	stamp := time.Unix(1234567, 0)
	fs := metadataFilesystem(t, metadataLayer(t,
		tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 02755, Uid: 1000, Gid: 1001, ModTime: stamp},
		tar.Header{Name: "private", Typeflag: tar.TypeDir, Mode: 02700, Uid: 1000, Gid: 1001},
		tar.Header{Name: "private/tool", Typeflag: tar.TypeReg, Mode: 04755, Uid: 1000, Gid: 1001, Size: 1, Xattrs: map[string]string{"user.test": "hello", "security.capability": string([]byte{0, 1, 0, 2})}},
		tar.Header{Name: "private/unreadable", Typeflag: tar.TypeReg, Mode: 0, Uid: 2000, Gid: 2001, Size: 1},
		tar.Header{Name: "tmp", Typeflag: tar.TypeDir, Mode: 01777},
		tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Mode: 0777, Uid: 3000, Gid: 3001, Linkname: "../../private/tool"},
	))
	headers := filesystemHeaders(t, fs)
	if h := headers["."]; h.Uid != 1000 || h.Gid != 1001 || h.Mode != 02755 || !h.ModTime.Equal(stamp) {
		t.Fatalf("root: %+v", h)
	}
	if h := headers["private/tool"]; h.Uid != 1000 || h.Gid != 1001 || h.Mode != 04755 || h.Xattrs["user.test"] != "hello" || h.Xattrs["security.capability"] != string([]byte{0, 1, 0, 2}) {
		t.Fatalf("tool: %+v", h)
	}
	if headers["private/unreadable"].Mode != 0 || headers["tmp"].Mode != 01777 || headers["link"].Uid != 3000 {
		t.Fatal("metadata changed")
	}
	entries, err := os.ReadDir(fs.scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("scratch has %d entries; only content files belong here", len(entries))
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe scratch mode %s", info.Mode())
		}
		if err := os.Chtimes(filepath.Join(fs.scratch, e.Name()), time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(headers, filesystemHeaders(t, fs)) {
		t.Fatal("host timestamps leaked")
	}
}
func TestFilesystemWhiteoutsAndHardlinksRetainInodeMetadata(t *testing.T) {
	fs := metadataFilesystem(t,
		metadataLayer(t,
			tar.Header{Name: "old", Typeflag: tar.TypeReg, Mode: 0600, Uid: 1000, Size: 1},
			tar.Header{Name: "alias", Typeflag: tar.TypeLink, Linkname: "old"},
			tar.Header{Name: "dir/old", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
			tar.Header{Name: "dir/nested/old", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		), metadataLayer(t,
			tar.Header{Name: ".wh.old", Typeflag: tar.TypeReg},
			tar.Header{Name: "dir/new", Typeflag: tar.TypeReg, Mode: 0600, Uid: 2000, Size: 1},
			tar.Header{Name: "dir/nested/new", Typeflag: tar.TypeReg, Mode: 0600, Uid: 2000, Size: 1},
			tar.Header{Name: "dir/.wh..wh..opq", Typeflag: tar.TypeReg},
			tar.Header{Name: "second-alias", Typeflag: tar.TypeLink, Linkname: "alias"},
		))
	h := filesystemHeaders(t, fs)
	for _, name := range []string{"old", "dir/old", "dir/nested/old"} {
		if _, ok := h[name]; ok {
			t.Fatalf("whiteout retained %s", name)
		}
	}
	if h["alias"].Uid != 1000 || h["alias"].Typeflag != tar.TypeReg || h["second-alias"].Linkname != "alias" || h["dir/nested/new"].Uid != 2000 {
		t.Fatalf("merged headers: %+v", h)
	}
	if n, err := fs.LogicalBytes(); err != nil || n != 3 {
		t.Fatalf("bytes=%d err=%v", n, err)
	}
	entries, err := os.ReadDir(fs.scratch)
	if err != nil || len(entries) != 3 {
		t.Fatalf("obsolete contents retained: %d %v", len(entries), err)
	}
}
func TestFilesystemReplacingHardlinkDoesNotChangeSurvivingAlias(t *testing.T) {
	fs := metadataFilesystem(t, metadataLayer(t, tar.Header{Name: "a", Typeflag: tar.TypeReg, Uid: 1000, Mode: 0600, Size: 1}, tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"}), metadataLayer(t, tar.Header{Name: "a", Typeflag: tar.TypeReg, Uid: 2000, Mode: 0400, Size: 2}))
	h := filesystemHeaders(t, fs)
	if h["a"].Uid != 2000 || h["b"].Uid != 1000 || h["b"].Typeflag != tar.TypeReg {
		t.Fatalf("aliases: %+v", h)
	}
}
func TestFilesystemRejectsInvalidMetadataAndParents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []tar.Header
	}{
		{"negative owner", []tar.Header{{Name: "file", Typeflag: tar.TypeReg, Uid: -1}}},
		{"reserved owner", []tar.Header{{Name: "file", Typeflag: tar.TypeReg, Uid: 4294967295}}},
		{"traversal", []tar.Header{{Name: "../escape", Typeflag: tar.TypeReg}}},
		{"symlink parent", []tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "link/child", Typeflag: tar.TypeReg}}},
		{"unknown semantic metadata", []tar.Header{{Name: "file", Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.acl.access": "user::rwx"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &Filesystem{scratch: t.TempDir(), entries: map[string]*imageInode{".": {header: tar.Header{Typeflag: tar.TypeDir, Mode: 0755}}}}
			if err := fs.applyLayer(bytes.NewReader(metadataLayer(t, tc.headers...)), ""); err == nil {
				t.Fatal("invalid image accepted")
			}
		})
	}
}

func TestFilesystemOpaqueWhiteoutKeepsReauthoredHardlink(t *testing.T) {
	for _, first := range []bool{false, true} {
		t.Run(fmt.Sprint("whiteout-first=", first), func(t *testing.T) {
			upper := []tar.Header{{Name: "dir/link", Typeflag: tar.TypeLink, Linkname: "outside"}, {Name: "dir/.wh..wh..opq", Typeflag: tar.TypeReg}}
			if first {
				upper[0], upper[1] = upper[1], upper[0]
			}
			fs := metadataFilesystem(t,
				metadataLayer(t, tar.Header{Name: "outside", Typeflag: tar.TypeReg, Mode: 0600, Uid: 1000, Size: 1}, tar.Header{Name: "dir/link", Typeflag: tar.TypeLink, Linkname: "outside"}), metadataLayer(t, upper...),
			)
			if _, ok := filesystemHeaders(t, fs)["dir/link"]; !ok {
				t.Fatal("opaque whiteout deleted upper hardlink")
			}
			if fs.entries["dir/link"] != fs.entries["outside"] {
				t.Fatal("hardlink inode identity lost")
			}
		})
	}
}

func TestFilesystemUsesTarTypeAndPreservesUnixModeBits(t *testing.T) {
	fs := metadataFilesystem(t, metadataLayer(t, tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0104755, Uid: 1000, Size: 1}))
	h := filesystemHeaders(t, fs)["file"]
	if h.Typeflag != tar.TypeReg || h.Mode != 04755 {
		t.Fatalf("file: %+v", h)
	}
}
