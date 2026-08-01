package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

func TestBuildTreeImageSourceIsCanonicalAndExact(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("README.md", []byte("not selected\n"), 0o644)
	tree.addDirectory("node_modules")
	tree.addDirectory("node_modules/tool")
	tree.addFile("node_modules/tool/index.js", []byte("dependency\n"), 0o644)
	tree.addDirectory("packages")
	tree.addDirectory("packages/app")
	tree.addFile("packages/app/main.js", []byte("main\n"), 0o755)
	frozen := testFrozenBuildTree(t, tree)

	plan := imageSourcePlan(
		&imagebuild.CopySourceFile{Path: "packages/app/main.js", Dst: "/app/main.js"},
		&imagebuild.CopySourceDir{Path: "node_modules/tool", Dst: "/app/tool"},
	)
	selection, err := frozen.SelectImageSource(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []imagebuild.SourcePath{
		{Path: "node_modules", Kind: imagebuild.SourcePathDirectory},
		{Path: "node_modules/tool", Kind: imagebuild.SourcePathDirectory},
		{Path: "node_modules/tool/index.js", Kind: imagebuild.SourcePathFile},
		{Path: "packages", Kind: imagebuild.SourcePathDirectory},
		{Path: "packages/app", Kind: imagebuild.SourcePathDirectory},
		{Path: "packages/app/main.js", Kind: imagebuild.SourcePathFile},
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
		descriptor.PathSetDigest != imagebuild.PathSetDigest(wantPaths) {
		t.Fatalf("source descriptor = %+v", descriptor)
	}
	first := writeSelectedSourceForTest(t, selection)
	second := writeSelectedSourceForTest(t, selection)
	if !bytes.Equal(first, second) || digestBytes(first) != descriptor.ArchiveDigest ||
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
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("app\n"), 0o644)
	tree.addDirectory("node_modules")
	tree.addDirectory("node_modules/tool")
	tree.addLink("node_modules/tool/current", "index.js")
	tree.addFile("node_modules/tool/index.js", []byte("dependency\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)

	selection, err := frozen.SelectImageSource(
		context.Background(),
		imageSourcePlan(nil, &imagebuild.CopySourceDir{Path: ".", Dst: "/app"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := selection.Paths()
	if err != nil {
		t.Fatal(err)
	}
	want := []imagebuild.SourcePath{
		{Path: "app.js", Kind: imagebuild.SourcePathFile},
		{Path: "node_modules", Kind: imagebuild.SourcePathDirectory},
		{Path: "node_modules/tool", Kind: imagebuild.SourcePathDirectory},
		{Path: "node_modules/tool/current", Kind: imagebuild.SourcePathSymlink},
		{Path: "node_modules/tool/index.js", Kind: imagebuild.SourcePathFile},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("root selection = %#v, want %#v", paths, want)
	}
}

func TestBuildTreeImageSourceRejectsReservedMissingAndWrongKindRoots(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("file.txt", []byte("file\n"), 0o644)
	tree.addDirectory("directory")
	frozen := testFrozenBuildTree(t, tree)

	tests := []struct {
		name string
		file *imagebuild.CopySourceFile
		dir  *imagebuild.CopySourceDir
		want string
	}{
		{name: "reserved file", file: &imagebuild.CopySourceFile{Path: "helmr/config.json", Dst: "/x"}, want: "clean Deployment-relative POSIX path"},
		{name: "reserved directory", dir: &imagebuild.CopySourceDir{Path: "helmr", Dst: "/x"}, want: "clean Deployment-relative POSIX path"},
		{name: "missing file", file: &imagebuild.CopySourceFile{Path: "missing", Dst: "/x"}, want: "missing"},
		{name: "file is directory", file: &imagebuild.CopySourceFile{Path: "directory", Dst: "/x"}, want: "want \"regular\""},
		{name: "directory is file", dir: &imagebuild.CopySourceDir{Path: "file.txt", Dst: "/x"}, want: "want \"directory\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := frozen.SelectImageSource(
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
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("app\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)
	selection, err := frozen.SelectImageSource(context.Background(), imageSourcePlan(nil, nil))
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
	tree := newMemoryArtifact()
	tree.addFile("app.js", []byte("app\n"), 0o644)
	frozen := testFrozenBuildTree(t, tree)
	descriptor, err := frozen.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	want := BuildTreeDescriptor{Digest: testDigest("build-tree-stream"), SizeBytes: 4096}
	if descriptor != want {
		t.Fatalf("BuildTree descriptor = %+v, want %+v", descriptor, want)
	}
	if err := frozen.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := frozen.Descriptor(); err == nil {
		t.Fatal("closed BuildTree returned a descriptor")
	}
}

func testFrozenBuildTree(t *testing.T, memory *memoryArtifact) *BuildTree {
	t.Helper()
	inspected, err := inspectMemoryBuildTree(t, memory)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := newBuildTree(
		&artifactSnapshot{},
		inspected,
		BuildTreeDescriptor{Digest: testDigest("build-tree-stream"), SizeBytes: 4096},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree
}

func imageSourcePlan(
	file *imagebuild.CopySourceFile,
	directory *imagebuild.CopySourceDir,
) imagebuild.Build {
	steps := []imagebuild.Step{{From: &imagebuild.From{Ref: "alpine:3.23"}}}
	if file != nil {
		steps = append(steps, imagebuild.Step{CopySourceFile: file})
	}
	if directory != nil {
		steps = append(steps, imagebuild.Step{CopySourceDir: directory})
	}
	return imagebuild.Build{
		FormatVersion: imagebuild.FormatVersion,
		Root:          "base",
		Images: []imagebuild.Spec{{
			Key:      "base",
			Platform: imagebuild.Platform{OS: "linux", Architecture: "x86_64"},
			Steps:    steps,
		}},
	}
}

func writeSelectedSourceForTest(t *testing.T, source *BuildTreeSource) []byte {
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
