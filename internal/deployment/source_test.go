package deployment

import (
	"archive/tar"
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestInspectSourcePinsExactManagerAndLockfile(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		manager  PackageManager
		lockfile string
	}{
		{
			name: "npm",
			files: map[string]string{
				"package.json":      `{"packageManager":"npm@11.4.2"}`,
				"package-lock.json": `{"lockfileVersion":3}`,
				"bun.lock":          "ordinary source",
			},
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			lockfile: "package-lock.json",
		},
		{
			name: "bun text",
			files: map[string]string{
				"package.json": `{"packageManager":"bun@1.3.11"}`,
				"bun.lock":     "lockfileVersion = 1",
			},
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			lockfile: "bun.lock",
		},
		{
			name: "bun binary",
			files: map[string]string{
				"package.json": `{"packageManager":"bun@1.3.11"}`,
				"bun.lockb":    "\x00binary",
			},
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			lockfile: "bun.lockb",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := InspectSource(sourceTar(t, test.files, nil))
			if err != nil {
				t.Fatal(err)
			}
			if selection.Manager != test.manager || selection.LockfileName != test.lockfile {
				t.Fatalf("selection = %#v", selection)
			}
			if selection.LockfileDigest != digestBytes([]byte(test.files[test.lockfile])) {
				t.Fatalf("lockfile digest = %q", selection.LockfileDigest)
			}
		})
	}
}

func TestInspectSourceRejectsAmbiguousOrInvalidAuthority(t *testing.T) {
	tests := map[string]map[string]string{
		"missing package manager": {
			"package.json": `{}`,
			"bun.lock":     "lock",
		},
		"manager range": {
			"package.json": `{"packageManager":"bun@^1.3.11"}`,
			"bun.lock":     "lock",
		},
		"missing selected lockfile": {
			"package.json": `{"packageManager":"npm@11.4.2"}`,
			"bun.lock":     "lock",
		},
		"two Bun lockfiles": {
			"package.json": `{"packageManager":"bun@1.3.11"}`,
			"bun.lock":     "text",
			"bun.lockb":    "binary",
		},
		"reserved output": {
			"package.json": `{"packageManager":"bun@1.3.11"}`,
			"bun.lock":     "lock",
			"helmr/output": "reserved",
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, files, nil)); err == nil {
				t.Fatal("InspectSource returned nil error")
			}
		})
	}
}

func TestInspectSourceAllowsNestedReservedNames(t *testing.T) {
	files := map[string]string{
		"package.json":                 `{"packageManager":"bun@1.3.11"}`,
		"bun.lock":                     "lock",
		"packages/tool/helmr/value.ts": "export {}",
		"packages/tool/node_modules/x": "x",
	}
	if _, err := InspectSource(sourceTar(t, files, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectSourceRejectsDuplicatePathsAndEscapingLinks(t *testing.T) {
	duplicate := sourceTar(t, map[string]string{
		"package.json": `{"packageManager":"bun@1.3.11"}`,
		"bun.lock":     "lock",
	}, []tar.Header{{Name: "bun.lock", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}})
	if _, err := InspectSource(duplicate); err == nil {
		t.Fatal("InspectSource accepted a duplicate path")
	}

	escaping := sourceTar(t, map[string]string{
		"package.json": `{"packageManager":"bun@1.3.11"}`,
		"bun.lock":     "lock",
	}, []tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}})
	if _, err := InspectSource(escaping); err == nil {
		t.Fatal("InspectSource accepted an escaping link")
	}
}

func sourceTar(t *testing.T, files map[string]string, extra []tar.Header) *bytes.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := files[name]
		header := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(value)),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range extra {
		header := value
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := writer.Write([]byte(strings.Repeat("x", int(header.Size)))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(body.Bytes())
}
