package artifact

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

func ValidateBuildTreeLinks(tree *Tree) error {
	for _, entry := range tree.Entries() {
		if entry.Kind != EntrySymlink {
			continue
		}
		if err := validateBuildTreeLink(tree, entry.Path, entry.LinkTarget); err != nil {
			return fmt.Errorf("build tree link %q: %w", entry.Path, err)
		}
	}
	return nil
}

func validateBuildTreeLink(
	tree *Tree,
	link string,
	target string,
) error {
	pending := append(
		strings.Split(path.Dir(link), "/"),
		strings.Split(target, "/")...,
	)
	resolved := make([]string, 0, len(pending))
	hops := 1 // Include the link whose target is being validated.
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return errors.New("target escapes the frozen project tree")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(resolved, component), "/")
		if err := safepath.ValidateTreePath(candidate, ProgramMountPath, "/workspace/project", "/workspace/program"); err != nil {
			return err
		}
		entry, exists := tree.Lookup(candidate)
		if !exists {
			return nil
		}
		if entry.Kind == EntrySymlink {
			hops++
			if hops > safepath.TreeLinkHops {
				return fmt.Errorf("target exceeds %d symbolic-link hops", safepath.TreeLinkHops)
			}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		if entry.Kind != EntryDirectory && len(pending) != 0 {
			return nil
		}
		resolved = append(resolved, component)
	}
	return nil
}
