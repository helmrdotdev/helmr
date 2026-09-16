package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInstalledPAXNamesLinksOrderingAndDeterminism(t *testing.T) {
	root := t.TempDir()
	files := []string{"a-", "a/child", "日本語.txt", "node_modules/pkg/" + strings.Repeat("x", 101), "node_modules/pkg/" + strings.Repeat("y", 124), "node_modules/pkg/" + strings.Repeat("z", 255), strings.Repeat("d", 80) + "/" + strings.Repeat("e", 80) + "/file"}
	for _, name := range files {
		writeTestFile(t, filepath.Join(root, name), name)
	}
	target := "../pkg/" + strings.Repeat("z", 255)
	if err := os.MkdirAll(filepath.Join(root, "node_modules/.bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "node_modules/.bin/tool")); err != nil {
		t.Fatal(err)
	}
	options := TarOptions{CanonicalMetadata: true, MaxBytes: 1 << 20, MaxEntries: 100, MaxNameBytes: 1 << 20, MaxArchiveBytes: 1 << 20}
	first, cleanup, err := CreateTarWithOptions(root, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	firstBody, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if err := os.Chmod(filepath.Join(root, name), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, name), time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	second, cleanup2, err := CreateTarWithOptions(root, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	secondBody, err := os.ReadFile(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBody, secondBody) || first.Digest != second.Digest {
		t.Fatal("installed PAX bytes changed with host times")
	}
	reader := tar.NewReader(bytes.NewReader(firstBody))
	var names []string
	found := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		found[header.Name] = true
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || !header.ModTime.Equal(time.Unix(0, 0)) || !header.AccessTime.IsZero() || !header.ChangeTime.IsZero() {
			t.Fatalf("nonnormalized metadata: %+v", header)
		}
		if header.Name == "node_modules/.bin/tool" && header.Linkname != target {
			t.Fatal("long link lost")
		}
		if header.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != header.Name {
				t.Fatalf("payload %q: %v", header.Name, err)
			}
		}
	}
	if !slices.IsSorted(names) || names[0] != "a-" || names[1] != "a/" {
		t.Fatalf("unstable ordering: %v", names)
	}
	for _, name := range files {
		if !found[name] {
			t.Fatalf("missing %q", name)
		}
	}
}

func TestInstalledStreamBudgetsIncludePAXAndFinalPadding(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 32; i++ {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("%02d", i)+strings.Repeat("x", 122)), "")
	}
	temp := t.TempDir()
	options := TarOptions{CanonicalMetadata: true, MaxBytes: 1, MaxEntries: 32, MaxNameBytes: 128 << 20, MaxArchiveBytes: 1 << 20}
	result, cleanup, err := CreateTarWithOptions(root, temp, options)
	if err != nil {
		t.Fatal(err)
	}
	size := result.SizeBytes
	if size <= 32*512+1024 {
		t.Fatal("fixture did not require PAX overhead")
	}
	cleanup()
	for _, test := range []struct {
		name   string
		change func(*TarOptions)
		want   string
	}{
		{"physical", func(o *TarOptions) { o.MaxArchiveBytes = size - 1 }, "encoded size limit"},
		{"names", func(o *TarOptions) { o.MaxNameBytes = 32 * 124 }, "name budget"},
		{"entries", func(o *TarOptions) { o.MaxEntries = 31 }, "too many entries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := options
			test.change(&o)
			if _, cleanup, err := CreateTarWithOptions(root, temp, o); err == nil {
				cleanup()
				t.Fatal("accepted over budget")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error: %v", err)
			}
			children, err := os.ReadDir(temp)
			if err != nil || len(children) != 0 {
				t.Fatal("failed stream retained archive")
			}
		})
	}
	options.MaxArchiveBytes = size
	if _, cleanup, err := CreateTarWithOptions(root, temp, options); err != nil {
		t.Fatal(err)
	} else {
		cleanup()
	}
}
