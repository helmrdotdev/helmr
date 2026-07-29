package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

type programVerifier struct {
	ctx      context.Context
	artifact *inspectedArtifact
	index    ProgramIndex
	manifest ProgramBuildManifest
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
	if err := verifyProgramBuildFiles(
		verifier.ctx,
		verifier.artifact,
		compilerResultFromManifest(verifier.manifest),
	); err != nil {
		return err
	}
	if err := verifyProgramBuildFile(
		verifier.ctx,
		verifier.artifact,
		verifier.manifest.ConfigSource,
	); err != nil {
		return err
	}
	if err := verifyProgramBuildFile(
		verifier.ctx,
		verifier.artifact,
		verifier.manifest.Lockfile,
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
		"helmr/build-manifest.json",
		maxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("Program build manifest: %w", err)
	}
	verifier.manifest, err = ParseProgramBuildManifest(manifestRaw)
	if err != nil {
		return fmt.Errorf("Program build manifest: %w", err)
	}
	verifier.index, err = ParseProgramIndex(indexRaw)
	if err != nil {
		return fmt.Errorf("program index: %w", err)
	}
	if verifier.manifest.Config.Digest != verifier.index.ConfigResultDigest {
		return fmt.Errorf(
			"Program build manifest config digest does not match Program index",
		)
	}
	indexHash := sha256.Sum256(indexRaw)
	if verifier.manifest.ProgramIndexDigest !=
		"sha256:"+hex.EncodeToString(indexHash[:]) {
		return fmt.Errorf(
			"Program build manifest index digest does not match Program index",
		)
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
	generated := make(map[string]struct{}, len(verifier.manifest.Outputs)*2)
	generatedDirectories := make(map[string]struct{}, len(verifier.manifest.Outputs)*2)
	for _, output := range verifier.manifest.Outputs {
		generated[output.ModulePath] = struct{}{}
		generated[output.SourceMapPath] = struct{}{}
		moduleDirectory := path.Dir(output.ModulePath)
		generatedDirectories[moduleDirectory] = struct{}{}
		generatedDirectories[path.Dir(moduleDirectory)] = struct{}{}
	}
	for _, required := range []string{".", "helmr", "node_modules"} {
		if _, err := verifier.artifact.require(required, artifactEntryDirectory); err != nil {
			return fmt.Errorf("Program layout: %w", err)
		}
	}
	for _, required := range []string{
		"helmr/build-manifest.json",
		"helmr/config.json",
		"helmr/declarations.json",
		"helmr/entry.mjs",
	} {
		if _, err := verifier.artifact.require(required, artifactEntryRegular); err != nil {
			return fmt.Errorf("Program layout: %w", err)
		}
	}
	for _, entry := range verifier.artifact.ordered {
		if strings.HasPrefix(entry.Path, "helmr/") {
			switch entry.Path {
			case "helmr/build-manifest.json", "helmr/config.json",
				"helmr/declarations.json", "helmr/entry.mjs":
			default:
				return fmt.Errorf(
					"Program Artifact contains unknown Platform-owned path %q",
					entry.Path,
				)
			}
			continue
		}
		if !hasReservedOutputSegment(entry.Path) {
			continue
		}
		if _, exists := generated[entry.Path]; exists &&
			entry.Kind == artifactEntryRegular {
			continue
		}
		if _, exists := generatedDirectories[entry.Path]; exists &&
			entry.Kind == artifactEntryDirectory {
			continue
		}
		return fmt.Errorf(
			"Program Artifact contains orphan generated path %q",
			entry.Path,
		)
	}
	return nil
}

func (verifier *programVerifier) verifyDeclarations() error {
	locator := DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations:  make([]LocatedDeclaration, 0),
	}
	for _, declaration := range verifier.index.Declarations {
		if declaration.Locator == nil {
			continue
		}
		if _, err := verifier.artifact.require(
			declaration.Locator.ModulePath,
			artifactEntryRegular,
		); err != nil {
			return fmt.Errorf(
				"declaration module %q: %w",
				declaration.Locator.ModulePath,
				err,
			)
		}
		locator.Declarations = append(locator.Declarations, LocatedDeclaration{
			DeclaredID: declaration.DeclaredID,
			ExportName: declaration.Locator.ExportName,
			Kind:       DeclarationKind(declaration.Kind),
			ModulePath: declaration.Locator.ModulePath,
			Slot:       declaration.Locator.Slot,
		})
	}
	return validateProgramBuildLocators(
		compilerResultFromManifest(verifier.manifest),
		locator,
	)
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
