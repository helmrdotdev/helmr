package deployment

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type pairVerifier struct {
	ctx       context.Context
	artifacts programArtifacts
	code      *inspectedArtifact
	deps      *inspectedArtifact
	index     ProgramIndex
	locator   DeclarationLocator
}

func (verifier *pairVerifier) verify() error {
	if err := verifier.readDocuments(); err != nil {
		return err
	}
	if err := verifier.verifyCodeLayout(); err != nil {
		return err
	}
	if err := verifier.verifyDependencyLayout(); err != nil {
		return err
	}
	if err := verifier.verifyDeclarations(); err != nil {
		return err
	}
	return verifier.verifyLinks()
}

func (verifier *pairVerifier) readDocuments() error {
	programRaw, err := verifier.code.read(verifier.ctx, "helmr/program.json", maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	verifier.index, err = ParseProgramIndex(programRaw)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	if verifier.index.DependenciesDigest != verifier.artifacts.Dependencies.Digest {
		return fmt.Errorf("program index dependenciesDigest does not match the dependency Artifact")
	}

	declarationsRaw, err := verifier.code.read(
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

	entryRaw, err := verifier.code.read(
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

func (verifier *pairVerifier) verifyCodeLayout() error {
	for _, required := range []string{".", "helmr", "node_modules"} {
		if _, err := verifier.code.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("code layout: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/declarations.json",
		"helmr/entry.mjs",
		"helmr/program.json",
	} {
		if _, err := verifier.code.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("code layout: %w", err)
		}
	}
	for _, entry := range verifier.code.ordered {
		if strings.HasPrefix(entry.Path, "node_modules/") {
			return fmt.Errorf("code Artifact dependency mountpoint is not empty at %q", entry.Path)
		}
		if strings.HasPrefix(entry.Path, "helmr/") {
			switch entry.Path {
			case "helmr/declarations.json", "helmr/entry.mjs", "helmr/program.json":
			default:
				return fmt.Errorf("code Artifact contains unknown Platform-owned path %q", entry.Path)
			}
		}
	}
	return nil
}

func (verifier *pairVerifier) verifyDependencyLayout() error {
	if _, err := verifier.deps.require(".", artifactEntryDirectory); err != nil {
		return fmt.Errorf("dependency layout: %w", err)
	}
	return nil
}

func (verifier *pairVerifier) verifyDeclarations() error {
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
		if _, err := verifier.code.require(located.ModulePath, artifactEntryRegular); err != nil {
			return fmt.Errorf(
				"declaration locator module %q: %w",
				located.ModulePath,
				err,
			)
		}
	}
	return nil
}

func (verifier *pairVerifier) verifyLinks() error {
	for _, entry := range verifier.code.ordered {
		if entry.Kind == artifactEntrySymlink {
			if err := verifier.verifyLink(entry.Path, entry.LinkTarget); err != nil {
				return fmt.Errorf("code link %q: %w", entry.Path, err)
			}
		}
	}
	for _, entry := range verifier.deps.ordered {
		if entry.Kind == artifactEntrySymlink {
			link := "node_modules"
			if entry.Path != "." {
				link += "/" + entry.Path
			}
			if err := verifier.verifyLink(link, entry.LinkTarget); err != nil {
				return fmt.Errorf("dependency link %q: %w", entry.Path, err)
			}
		}
	}
	return nil
}

func (verifier *pairVerifier) verifyLink(link, target string) error {
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
				return fmt.Errorf("target escapes the combined Program namespace")
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(resolved, component), "/")
		entry, exists := verifier.combinedEntry(candidate)
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

func (verifier *pairVerifier) combinedEntry(name string) (artifactEntry, bool) {
	if name == "node_modules" {
		entry, exists := verifier.code.entries[name]
		return entry, exists
	}
	if strings.HasPrefix(name, "node_modules/") {
		entry, exists := verifier.deps.entries[strings.TrimPrefix(name, "node_modules/")]
		return entry, exists
	}
	entry, exists := verifier.code.entries[name]
	return entry, exists
}
