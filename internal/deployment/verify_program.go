package deployment

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type programVerifier struct {
	ctx      context.Context
	artifact *inspectedArtifact
	index    ProgramIndex
	locator  DeclarationLocator
}

func (verifier *programVerifier) verify() error {
	if err := verifier.readDocuments(); err != nil {
		return err
	}
	if err := verifier.verifyLayout(); err != nil {
		return err
	}
	if err := verifier.verifyDeclarations(); err != nil {
		return err
	}
	return verifier.verifyLinks()
}

func (verifier *programVerifier) readDocuments() error {
	programRaw, err := verifier.artifact.read(
		verifier.ctx,
		"helmr/program.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	verifier.index, err = ParseProgramIndex(programRaw)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}

	declarationsRaw, err := verifier.artifact.read(
		verifier.ctx,
		"helmr/declarations.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("declaration locator: %w", err)
	}
	verifier.locator, err = ParseDeclarationLocator(declarationsRaw)
	if err != nil {
		return fmt.Errorf("declaration locator: %w", err)
	}

	entryRaw, err := verifier.artifact.read(
		verifier.ctx,
		"helmr/entry.mjs",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return err
	}
	if string(entryRaw) != ProgramEntry {
		return fmt.Errorf("helmr/entry.mjs does not match the fixed Program entry")
	}
	return nil
}

func (verifier *programVerifier) verifyLayout() error {
	for _, required := range []string{".", "helmr", "node_modules"} {
		if _, err := verifier.artifact.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("Program layout: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/declarations.json",
		"helmr/entry.mjs",
		"helmr/program.json",
	} {
		if _, err := verifier.artifact.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("Program layout: %w", err)
		}
	}
	for _, entry := range verifier.artifact.ordered {
		if strings.HasPrefix(entry.Path, "helmr/") {
			switch entry.Path {
			case "helmr/declarations.json", "helmr/entry.mjs", "helmr/program.json":
			default:
				return fmt.Errorf(
					"Program Artifact contains unknown Platform-owned path %q",
					entry.Path,
				)
			}
		}
	}
	return nil
}

func (verifier *programVerifier) verifyDeclarations() error {
	if len(verifier.locator.Declarations) != len(verifier.index.Declarations) {
		return fmt.Errorf(
			"declaration locator count %d does not match program index count %d",
			len(verifier.locator.Declarations),
			len(verifier.index.Declarations),
		)
	}
	for index, located := range verifier.locator.Declarations {
		projection := verifier.index.Declarations[index]
		if located.Kind != projection.Kind ||
			located.DeclaredID != projection.DeclaredID {
			return fmt.Errorf(
				"declaration locator identity at position %d does not match program index",
				index,
			)
		}
		if _, err := verifier.artifact.require(located.ModulePath, artifactEntryRegular); err != nil {
			return fmt.Errorf("declaration locator module %q: %w", located.ModulePath, err)
		}
	}
	return nil
}

func (verifier *programVerifier) verifyLinks() error {
	for _, entry := range verifier.artifact.ordered {
		if entry.Kind == artifactEntrySymlink {
			if err := verifier.verifyLink(entry.Path, entry.LinkTarget); err != nil {
				return fmt.Errorf("Program link %q: %w", entry.Path, err)
			}
		}
	}
	return nil
}

func (verifier *programVerifier) verifyLink(link, target string) error {
	pending := append(
		strings.Split(path.Dir(link), "/"),
		strings.Split(target, "/")...,
	)
	resolved := make([]string, 0, len(pending))
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return fmt.Errorf("target escapes the Program namespace")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(resolved, component), "/")
		entry, exists := verifier.artifact.entries[candidate]
		if !exists {
			return nil
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return fmt.Errorf("target exceeds %d symbolic-link hops", maxSymlinkHops)
			}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		if entry.Kind != artifactEntryDirectory && len(pending) != 0 {
			return nil
		}
		resolved = append(resolved, component)
	}
	return nil
}
