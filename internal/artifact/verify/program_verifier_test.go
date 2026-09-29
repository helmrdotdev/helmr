package verify

import (
	"fmt"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func TestProgramLinkHopBoundary(t *testing.T) {
	for _, count := range []int{40, 41} {
		tree := newMemoryArtifact()
		tree.addFile("file", []byte("content"), 0644)
		for i := 0; i < count; i++ {
			target := "file"
			if i+1 < count {
				target = fmt.Sprintf("link-%02d", i+1)
			}
			tree.addLink(fmt.Sprintf("link-%02d", i), target)
		}
		inspected, err := artifact.Inspect(t.Context(), tree, artifact.RoleProgram, artifact.SquashFSPhysicalAlign)
		if err != nil {
			t.Fatal(err)
		}
		verifier := programVerifier{artifact: inspected}
		err = verifier.verifyLinks()
		if (err == nil) != (count == 40) {
			t.Fatalf("Program %d links: %v", count, err)
		}
	}
}
