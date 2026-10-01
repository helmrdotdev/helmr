package builder

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

func TestBuildTreeAcceptsManagerNativeOutput(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddFile("package.json", []byte(`{"packageManager":"bun@1.3.13"}`), 0644)
	tree.AddDirectory("node_modules")
	tree.AddDirectory("node_modules/.bin")
	tree.AddDirectory("node_modules/tool")
	tree.AddFile("node_modules/tool/index.js", []byte("export {}\n"), 0644)
	tree.AddLink("node_modules/.bin/tool", "../tool/index.js")

	if _, err := inspectMemoryBuildTree(t, tree); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTreeRejectsReservedOrInvalidRoots(t *testing.T) {
	tests := map[string]func(*artifacttest.Memory){
		"generated output": func(tree *artifacttest.Memory) {
			tree.AddDirectory("helmr")
		},
		"dependency file": func(tree *artifacttest.Memory) {
			tree.AddFile("node_modules", []byte("not a directory"), 0644)
		},
		"escaping link": func(tree *artifacttest.Memory) {
			tree.AddLink("escape", "../outside")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tree := artifacttest.NewMemory()
			mutate(tree)
			if _, err := inspectMemoryBuildTree(t, tree); err == nil {
				t.Fatal("build tree was accepted")
			}
		})
	}
}

func TestBuildTreeAcceptsConfinedDanglingAndNestedReservedNames(t *testing.T) {
	tree := artifacttest.NewMemory()
	tree.AddDirectory("packages")
	tree.AddDirectory("packages/app")
	tree.AddDirectory("packages/app/helmr")
	tree.AddDirectory("packages/app/node_modules")
	tree.AddLink("packages/current", "missing")

	if _, err := inspectMemoryBuildTree(t, tree); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTreeRejectsLinkCyclePastBound(t *testing.T) {
	tree := artifacttest.NewMemory()
	for index := 0; index <= safepath.TreeLinkHops; index++ {
		name := "link-" + strings.Repeat("x", index)
		target := "link-" + strings.Repeat("x", index+1)
		tree.AddLink(name, target)
	}
	tree.AddLink(
		"link-"+strings.Repeat("x", safepath.TreeLinkHops+1),
		"link-",
	)
	if _, err := inspectMemoryBuildTree(t, tree); err == nil {
		t.Fatal("build tree link cycle was accepted")
	}
}

func inspectMemoryBuildTree(
	t *testing.T,
	tree *artifacttest.Memory,
) (*artifact.Tree, error) {
	t.Helper()
	inspected, err := artifact.Inspect(
		context.Background(),
		tree,
		artifact.RoleBuildTree,
		artifact.SquashFSPhysicalAlign,
	)
	if err != nil {
		return nil, err
	}
	if err := validateInspectedBuildTree(context.Background(), inspected); err != nil {
		return nil, err
	}
	return inspected, nil
}

func TestBuildTreeLinkHopBoundary(t *testing.T) {
	for _, count := range []int{40, 41} {
		tree := artifacttest.NewMemory()
		tree.AddFile("file", []byte("content"), 0644)
		for i := 0; i < count; i++ {
			target := "file"
			if i+1 < count {
				target = fmt.Sprintf("link-%02d", i+1)
			}
			tree.AddLink(fmt.Sprintf("link-%02d", i), target)
		}
		_, err := inspectMemoryBuildTree(t, tree)
		if (err == nil) != (count == 40) {
			t.Fatalf("%d links: %v", count, err)
		}
	}
}
