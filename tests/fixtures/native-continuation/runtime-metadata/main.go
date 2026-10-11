// Command runtime-metadata refreshes fixture entry metadata from a pinned Runtime.
// It does not certify a release artifact.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func main() {
	if len(os.Args) != 2 {
		panic("usage: runtime-metadata runtime-tree")
	}
	root := os.Args[1]
	raw, err := os.ReadFile(filepath.Join(root, "helmr/runtime.json"))
	must(err)
	metadata, err := artifact.ParseRuntimeMetadata(raw)
	must(err)
	preload, err := os.ReadFile(filepath.Join(root, "helmr/module-preload.mjs"))
	must(err)
	metadata.ModulePolicyDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(preload))
	raw, err = artifact.CanonicalRuntimeMetadata(metadata)
	must(err)
	must(os.WriteFile(filepath.Join(root, "helmr/runtime.json"), raw, 0644))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
