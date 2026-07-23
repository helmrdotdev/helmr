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

func TestProgramTreeEntriesPartitionOneFrozenTree(t *testing.T) {
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
		"helmr/declarations.json": []byte(`{"declarations":[]}`),
		"helmr/entry.mjs":         []byte("entry\n"),
		"helmr/program.json":      []byte(`{"formatVersion":0}`),
	}

	code := writeProgramTreeFixture(
		t,
		codeArtifact,
		programTreeEntries(
			context.Background(),
			inspected,
			codeArtifact,
			generated,
		),
		false,
	)
	dependencies := writeProgramTreeFixture(
		t,
		dependencyArtifact,
		programTreeEntries(
			context.Background(),
			inspected,
			dependencyArtifact,
			nil,
		),
		true,
	)

	wantCode := map[string]string{
		"app.js":                             "export const app = true\n",
		"helmr":                              "",
		"helmr/declarations.json":            `{"declarations":[]}`,
		"helmr/entry.mjs":                    "entry\n",
		"helmr/program.json":                 `{"formatVersion":0}`,
		"node_modules":                       "",
		"packages":                           "",
		"packages/app":                       "",
		"packages/app/node_modules":          "",
		"packages/app/node_modules/local.js": "nested\n",
	}
	if !maps.Equal(code, wantCode) {
		t.Fatalf("code tree = %#v, want %#v", code, wantCode)
	}
	wantDependencies := map[string]string{
		"tool":          "",
		"tool/index.js": "dependency\n",
	}
	if !maps.Equal(dependencies, wantDependencies) {
		t.Fatalf(
			"dependency tree = %#v, want %#v",
			dependencies,
			wantDependencies,
		)
	}
}

func TestProgramTreeEntriesEncodeAbsentDependenciesAsEmptyTree(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("export {}\n"), 0644)
	inspected, err := inspectMemoryBuildTree(t, tree)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := writeProgramTreeFixture(
		t,
		dependencyArtifact,
		programTreeEntries(
			context.Background(),
			inspected,
			dependencyArtifact,
			nil,
		),
		true,
	)
	if len(dependencies) != 0 {
		t.Fatalf("empty dependency tree = %#v", dependencies)
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
