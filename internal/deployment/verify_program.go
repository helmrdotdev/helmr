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
	manifest ProgramManifest
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
	if err := verifyProgramManifestFiles(
		verifier.ctx,
		verifier.artifact,
		verifier.manifest,
	); err != nil {
		return err
	}
	return verifier.verifyLinks()
}

func (verifier *programVerifier) readDocuments() error {
	indexRaw, err := verifier.artifact.read(
		verifier.ctx,
		"helmr/declarations.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	manifestRaw, err := verifier.artifact.read(
		verifier.ctx,
		"helmr/program-manifest.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("program manifest: %w", err)
	}
	verifier.manifest, err = ParseProgramManifest(manifestRaw)
	if err != nil {
		return fmt.Errorf("program manifest: %w", err)
	}
	verifier.index, err = ParseProgramIndex(indexRaw)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	if verifier.manifest.Config.Digest != verifier.index.ConfigResultDigest {
		return fmt.Errorf(
			"program manifest config digest does not match program index",
		)
	}
	if verifier.manifest.ProgramIndexDigest != programIndexDigest(indexRaw) {
		return fmt.Errorf(
			"program manifest index digest does not match program index",
		)
	}

	return nil
}

func (verifier *programVerifier) verifyLayout() error {
	for _, required := range []string{".", "helmr"} {
		if _, err := verifier.artifact.require(required, artifactEntryDirectory); err != nil {
			return err
		}
	}
	for _, required := range []string{"helmr/program-manifest.json", "helmr/config.json", "helmr/declarations.json"} {
		if _, err := verifier.artifact.require(required, artifactEntryRegular); err != nil {
			return err
		}
	}
	if entry, exists := verifier.artifact.entries["node_modules"]; exists && entry.Kind != artifactEntryDirectory {
		return fmt.Errorf("root node_modules must be a directory")
	}
	for _, entry := range verifier.artifact.ordered {
		if strings.HasPrefix(entry.Path, "helmr/") && entry.Path != "helmr/program-manifest.json" && entry.Path != "helmr/config.json" && entry.Path != "helmr/declarations.json" {
			return fmt.Errorf("unknown platform-owned path %q", entry.Path)
		}
	}
	return nil
}
func (verifier *programVerifier) verifyDeclarations() error {
	for _, declaration := range verifier.index.Declarations {
		if declaration.Locator == nil {
			continue
		}
		if err := verifyDeclarationSource(verifier.artifact, declaration.Locator.SourcePath); err != nil {
			return err
		}
	}
	return nil
}

func (verifier *programVerifier) verifyLinks() error {
	for _, entry := range verifier.artifact.ordered {
		if entry.Kind == artifactEntrySymlink {
			if err := verifier.verifyLink(entry.Path, entry.LinkTarget); err != nil {
				return fmt.Errorf("program link %q: %w", entry.Path, err)
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
				return fmt.Errorf("target escapes the program namespace")
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
