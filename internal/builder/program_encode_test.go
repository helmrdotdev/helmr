package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
)

func TestProgramTreeEntriesEncodeOneFrozenTree(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("app.js", []byte("export const app = true\n"), 0644)
	tree.AddDirectory("packages")
	tree.AddDirectory("packages/app")
	tree.AddDirectory("packages/app/node_modules")
	tree.AddFile("packages/app/node_modules/local.js", []byte("nested\n"), 0644)
	tree.AddDirectory("node_modules")
	tree.AddDirectory("node_modules/.bin")
	tree.AddDirectory("node_modules/tool")
	tree.AddFile("node_modules/tool/index.js", []byte("dependency\n"), 0644)
	tree.AddLink("node_modules/.bin/tool", "../tool/index.js")
	inspected, err := inspectMemoryBuildTree(t, tree)
	if err != nil {
		t.Fatal(err)
	}
	generated := map[string][]byte{
		"helmr/program-manifest.json": []byte(`{"modules":[]}`),
		"helmr/program-metadata.json": []byte(`{"declarations":[]}`),
		"helmr/entry.mjs":             []byte("entry\n"),
	}

	program := writeProgramTreeFixture(
		t,
		artifact.RoleProgram,
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
		"helmr/program-metadata.json":        `{"declarations":[]}`,
		"helmr/entry.mjs":                    "entry\n",
		"node_modules":                       "",
		"node_modules/.bin":                  "",
		"node_modules/.bin/tool":             "../tool/index.js",
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
	tree := artifacttest.NewMemory()
	tree.AddFile("app.js", []byte("export {}\n"), 0644)
	inspected, err := inspectMemoryBuildTree(t, tree)
	if err != nil {
		t.Fatal(err)
	}
	program := writeProgramTreeFixture(
		t,
		artifact.RoleProgram,
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
	role artifact.Role,
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
