package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"testing"
)

func TestProgramTreeEntriesEncodeOneFrozenTree(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("export const app = true\n"), 0644)
	tree.addDirectory("packages")
	tree.addDirectory("packages/app")
	tree.addDirectory("packages/app/node_modules")
	tree.addFile("packages/app/node_modules/local.js", []byte("nested\n"), 0644)
	tree.addDirectory("node_modules")
	tree.addDirectory("node_modules/tool")
	tree.addFile("node_modules/tool/index.js", []byte("dependency\n"), 0644)
	inspected, err := inspectMemoryBuildTree(t, tree)
	if err != nil {
		t.Fatal(err)
	}
	generated := map[string][]byte{
		"helmr/program-manifest.json": []byte(`{"modules":[]}`),
		"helmr/declarations.json":     []byte(`{"declarations":[]}`),
		"helmr/entry.mjs":             []byte("entry\n"),
	}

	program := writeProgramTreeFixture(
		t,
		programArtifact,
		programTreeEntries(
			context.Background(),
			inspected,
			generated,
		),
		false,
	)

	want := map[string]string{
		"app.js":                             "export const app = true\n",
		"helmr":                              "",
		"helmr/program-manifest.json":        `{"modules":[]}`,
		"helmr/declarations.json":            `{"declarations":[]}`,
		"helmr/entry.mjs":                    "entry\n",
		"node_modules":                       "",
		"node_modules/tool":                  "",
		"node_modules/tool/index.js":         "dependency\n",
		"packages":                           "",
		"packages/app":                       "",
		"packages/app/node_modules":          "",
		"packages/app/node_modules/local.js": "nested\n",
	}
	if !maps.Equal(program, want) {
		t.Fatalf("Program tree = %#v, want %#v", program, want)
	}
}

func TestProgramTreeEntriesCreateEmptyNodeModules(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("export {}\n"), 0644)
	inspected, err := inspectMemoryBuildTree(t, tree)
	if err != nil {
		t.Fatal(err)
	}
	program := writeProgramTreeFixture(
		t,
		programArtifact,
		programTreeEntries(
			context.Background(),
			inspected,
			nil,
		),
		false,
	)
	if program["node_modules"] != "" {
		t.Fatalf("Program tree = %#v", program)
	}
}

func writeProgramTreeFixture(
	t *testing.T,
	role artifactRole,
	entries func(func(treeEntry, error) bool),
	allowEmpty bool,
) map[string]string {
	t.Helper()
	var archive bytes.Buffer
	if err := writeTreeArchive(
		context.Background(),
		&archive,
		role,
		entries,
		allowEmpty,
	); err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Typeflag {
		case tar.TypeXHeader:
			continue
		case tar.TypeDir, tar.TypeSymlink:
			result[header.Name] = header.Linkname
		case tar.TypeReg:
			raw, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			result[header.Name] = string(raw)
		default:
			t.Fatalf("unexpected tar member %#v", header)
		}
	}
	return result
}
