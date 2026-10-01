package builder

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestBuildTreeImageSourceIsCanonicalAndExact(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("README.md", []byte("not selected\n"), 0o644)
	tree.AddDirectory("node_modules")
	tree.AddDirectory("node_modules/tool")
	tree.AddFile("node_modules/tool/index.js", []byte("dependency\n"), 0o644)
	tree.AddDirectory("packages")
	tree.AddDirectory("packages/app")
	tree.AddFile("packages/app/main.js", []byte("main\n"), 0o755)
	frozen := testFrozenBuildTree(t, tree)

	plan := imageSourcePlan(
		&definition.ImageCopySourceFile{Path: "packages/app/main.js", Dst: "/app/main.js"},
		&definition.ImageCopySourceDir{Path: "node_modules/tool", Dst: "/app/tool"},
	)
	selection, err := frozen.selectImageSource(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []sourceArchivePath{
		{Path: "node_modules", Kind: sourcePathDirectory},
		{Path: "node_modules/tool", Kind: sourcePathDirectory},
		{Path: "node_modules/tool/index.js", Kind: sourcePathFile},
		{Path: "packages", Kind: sourcePathDirectory},
		{Path: "packages/app", Kind: sourcePathDirectory},
		{Path: "packages/app/main.js", Kind: sourcePathFile},
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("selected paths = %#v, want %#v", paths, wantPaths)
	}
	paths[0].Path = "caller-mutated"
	pathsAgain, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pathsAgain, wantPaths) {
		t.Fatalf("selected paths changed through caller copy: %#v", pathsAgain)
	}

	descriptor, err := selection.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ArchiveEntries != len(wantPaths) ||
		descriptor.PathSetDigest != sourcePathSetDigest(wantPaths) {
		t.Fatalf("source descriptor = %+v", descriptor)
	}
	first := writeSelectedSourceForTest(t, selection)
	second := writeSelectedSourceForTest(t, selection)
	if !bytes.Equal(first, second) || sha256sum.DigestBytes(first) != descriptor.ArchiveDigest ||
		int64(len(first)) != descriptor.ArchiveSizeBytes {
		t.Fatalf("canonical archive descriptor = %+v", descriptor)
	}
	files := readSelectedSourceTar(t, first)
	wantFiles := map[string]string{
		"node_modules":               "",
		"node_modules/tool":          "",
		"node_modules/tool/index.js": "dependency\n",
		"packages":                   "",
		"packages/app":               "",
		"packages/app/main.js":       "main\n",
	}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("selected archive = %#v, want %#v", files, wantFiles)
	}
}

func TestBuildTreeImageSourceRootSelectionKeepsNodeModules(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("app.js", []byte("app\n"), 0o644)
	tree.AddDirectory("node_modules")
	tree.AddDirectory("node_modules/tool")
	tree.AddLink("node_modules/tool/current", "index.js")
	tree.AddFile("node_modules/tool/index.js", []byte("dependency\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)

	selection, err := frozen.selectImageSource(
		context.Background(),
		imageSourcePlan(nil, &definition.ImageCopySourceDir{Path: ".", Dst: "/app"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	want := []sourceArchivePath{
		{Path: "app.js", Kind: sourcePathFile},
		{Path: "node_modules", Kind: sourcePathDirectory},
		{Path: "node_modules/tool", Kind: sourcePathDirectory},
		{Path: "node_modules/tool/current", Kind: sourcePathSymlink},
		{Path: "node_modules/tool/index.js", Kind: sourcePathFile},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("root selection = %#v, want %#v", paths, want)
	}
}

func TestBuildTreeImageSourceRejectsReservedMissingAndWrongKindRoots(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("file.txt", []byte("file\n"), 0o644)
	tree.AddDirectory("directory")
	frozen := testFrozenBuildTree(t, tree)

	tests := []struct {
		name string
		file *definition.ImageCopySourceFile
		dir  *definition.ImageCopySourceDir
		want string
	}{
		{name: "reserved file", file: &definition.ImageCopySourceFile{Path: "helmr/config.json", Dst: "/x"}, want: "clean deployment-relative POSIX path"},
		{name: "reserved directory", dir: &definition.ImageCopySourceDir{Path: "helmr", Dst: "/x"}, want: "clean deployment-relative POSIX path"},
		{name: "missing file", file: &definition.ImageCopySourceFile{Path: "missing", Dst: "/x"}, want: "missing"},
		{name: "file is directory", file: &definition.ImageCopySourceFile{Path: "directory", Dst: "/x"}, want: "want \"regular\""},
		{name: "directory is file", dir: &definition.ImageCopySourceDir{Path: "file.txt", Dst: "/x"}, want: "want \"directory\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := frozen.selectImageSource(
				context.Background(),
				imageSourcePlan(test.file, test.dir),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("selection error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestBuildTreeImageSourceSupportsEmptySelection(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("app.js", []byte("app\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)
	selection, err := frozen.selectImageSource(context.Background(), imageSourcePlan(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := selection.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	archive := writeSelectedSourceForTest(t, selection)
	if paths == nil || len(paths) != 0 || descriptor.ArchiveEntries != 0 || len(archive) != 1024 {
		t.Fatalf("empty selection paths = %#v descriptor = %+v bytes = %d", paths, descriptor, len(archive))
	}
}

func TestBuildTreeDescriptorPreservesVerifiedStreamIdentity(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("app.js", []byte("app\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)
	descriptor, err := frozen.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	want := buildTreeDescriptor{Digest: artifacttest.Digest("build-tree-stream"), SizeBytes: 4096}
	if descriptor != want {
		t.Fatalf("buildTree descriptor = %+v, want %+v", descriptor, want)
	}
	if err := frozen.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := frozen.Descriptor(); err == nil {
		t.Fatal("closed buildTree returned a descriptor")
	}
}

func testFrozenBuildTree(t *testing.T, memory *artifacttest.Memory) *buildTree {
	t.Helper()
	inspected, err := inspectMemoryBuildTree(t, memory)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := newBuildTree(
		&snapshot.Artifact{},
		inspected,
		buildTreeDescriptor{Digest: artifacttest.Digest("build-tree-stream"), SizeBytes: 4096},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

func imageSourcePlan(
	file *definition.ImageCopySourceFile,
	directory *definition.ImageCopySourceDir,
) definition.ImageBuild {
	steps := []definition.ImageStep{{From: &definition.ImageFrom{Ref: "alpine:3.23"}}}
	if file != nil {
		steps = append(steps, definition.ImageStep{CopySourceFile: file})
	}
	if directory != nil {
		steps = append(steps, definition.ImageStep{CopySourceDir: directory})
	}
	return definition.ImageBuild{
		Root: "base",
		Images: []definition.ImageSpec{{
			Key:      "base",
			Platform: definition.ImagePlatform{OS: "linux", Architecture: "x86_64"},
			Steps:    steps,
		}},
	}
}

func writeSelectedSourceForTest(t *testing.T, source *buildTreeSource) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := source.WriteTo(context.Background(), &encoded); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func readSelectedSourceTar(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	result := make(map[string]string)
	reader := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			result[header.Name] = ""
		case tar.TypeSymlink:
			result[header.Name] = header.Linkname
		case tar.TypeReg:
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			result[header.Name] = string(content)
		default:
			t.Fatalf("unexpected tar type %d for %q", header.Typeflag, header.Name)
		}
	}
}
