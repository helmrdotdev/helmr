package deployment

import (
	"context"
	"strings"
	"testing"
)

func TestBuildTreeAcceptsManagerNativeOutput(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addFile("package.json", []byte(`{"packageManager":"bun@1.3.10"}`), 0644)
	tree.addDirectory("node_modules")
	tree.addDirectory("node_modules/.bin")
	tree.addDirectory("node_modules/tool")
	tree.addFile("node_modules/tool/index.js", []byte("export {}\n"), 0644)
	tree.addLink("node_modules/.bin/tool", "../tool/index.js")

	if _, err := inspectMemoryBuildTree(t, tree); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTreeRejectsReservedOrInvalidRoots(t *testing.T) {
	tests := map[string]func(*memoryArtifact){
		"generated output": func(tree *memoryArtifact) {
			tree.addDirectory("helmr")
		},
		"dependency file": func(tree *memoryArtifact) {
			tree.addFile("node_modules", []byte("not a directory"), 0644)
		},
		"escaping link": func(tree *memoryArtifact) {
			tree.addLink("escape", "../outside")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tree := newMemoryArtifact()
			mutate(tree)
			if _, err := inspectMemoryBuildTree(t, tree); err == nil {
				t.Fatal("build tree was accepted")
			}
		})
	}
}

func TestBuildTreeAcceptsConfinedDanglingAndNestedReservedNames(t *testing.T) {
	tree := newMemoryArtifact()
	tree.addDirectory("packages")
	tree.addDirectory("packages/app")
	tree.addDirectory("packages/app/helmr")
	tree.addDirectory("packages/app/node_modules")
	tree.addLink("packages/current", "missing")

	if _, err := inspectMemoryBuildTree(t, tree); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTreeRejectsLinkCyclePastBound(t *testing.T) {
	tree := newMemoryArtifact()
	for index := 0; index <= maxSymlinkHops; index++ {
		name := "link-" + strings.Repeat("x", index)
		target := "link-" + strings.Repeat("x", index+1)
		tree.addLink(name, target)
	}
	tree.addLink(
		"link-"+strings.Repeat("x", maxSymlinkHops+1),
		"link-",
	)
	if _, err := inspectMemoryBuildTree(t, tree); err == nil {
		t.Fatal("build tree link cycle was accepted")
	}
}

func inspectMemoryBuildTree(
	t *testing.T,
	tree *memoryArtifact,
) (*inspectedArtifact, error) {
	t.Helper()
	inspected, err := inspectArtifact(
		context.Background(),
		tree,
		buildTreeArtifact,
		maxBuildTreeLogicalBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		return nil, err
	}
	if err := validateInspectedBuildTree(context.Background(), inspected); err != nil {
		return nil, err
	}
	return inspected, nil
}
