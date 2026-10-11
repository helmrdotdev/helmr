package verify

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/safepath"
)

type programVerifier struct {
	ctx             context.Context
	artifact        *artifact.Tree
	index           artifact.ProgramMetadata
	manifest        artifact.ProgramManifest
	definitionIndex artifact.DefinitionIndex
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
	if err := artifact.VerifyProgramManifestFiles(
		verifier.ctx,
		verifier.artifact,
		verifier.manifest,
	); err != nil {
		return err
	}
	return verifier.verifyLinks()
}

func (verifier *programVerifier) readDocuments() error {
	indexRaw, err := verifier.artifact.Read(
		verifier.ctx,
		"helmr/program-metadata.json",
		artifact.MaxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("program metadata: %w", err)
	}
	manifestRaw, err := verifier.artifact.Read(
		verifier.ctx,
		"helmr/program-manifest.json",
		artifact.MaxProgramFileSizeBytes,
	)
	if err != nil {
		return fmt.Errorf("program manifest: %w", err)
	}
	verifier.manifest, err = artifact.ParseProgramManifest(manifestRaw)
	if err != nil {
		return fmt.Errorf("program manifest: %w", err)
	}
	verifier.index, err = artifact.ParseProgramMetadata(indexRaw)
	if err != nil {
		return fmt.Errorf("program metadata: %w", err)
	}
	if verifier.manifest.Config.Digest != verifier.index.ConfigResultDigest {
		return fmt.Errorf(
			"program manifest config digest does not match program metadata",
		)
	}
	if verifier.manifest.ProgramMetadataDigest != artifact.ProgramMetadataDigest(indexRaw) {
		return fmt.Errorf(
			"program manifest index digest does not match program metadata",
		)
	}

	agentRaw, err := verifier.artifact.Read(verifier.ctx, "helmr/definition-index.json", artifact.MaxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("definition index: %w", err)
	}
	verifier.definitionIndex, err = artifact.ParseDefinitionIndex(agentRaw)
	if err != nil {
		return fmt.Errorf("definition index: %w", err)
	}
	if err := artifact.ValidateProgramDefinitionIndex(verifier.index, verifier.definitionIndex); err != nil {
		return err
	}

	return nil
}

func (verifier *programVerifier) verifyLayout() error {
	for _, required := range []string{".", "helmr"} {
		if _, err := verifier.artifact.Require(required, artifact.EntryDirectory); err != nil {
			return err
		}
	}
	for _, required := range []string{"helmr/program-manifest.json", "helmr/config.json", "helmr/program-metadata.json", "helmr/definition-index.json"} {
		if _, err := verifier.artifact.Require(required, artifact.EntryRegular); err != nil {
			return err
		}
	}
	if entry, exists := verifier.artifact.Lookup("node_modules"); exists && entry.Kind != artifact.EntryDirectory {
		return fmt.Errorf("root node_modules must be a directory")
	}
	for _, entry := range verifier.artifact.Entries() {
		if strings.HasPrefix(entry.Path, "helmr/") && !artifact.IsGeneratedProgramEntry(entry) && entry.Path != "helmr/program-manifest.json" && entry.Path != "helmr/config.json" && entry.Path != "helmr/program-metadata.json" && entry.Path != "helmr/definition-index.json" {
			return fmt.Errorf("unknown platform-owned path %q", entry.Path)
		}
	}
	return nil
}
func (verifier *programVerifier) verifyDeclarations() error {
	for _, entry := range verifier.definitionIndex.Agents {
		if err := artifact.VerifyDeclarationModule(verifier.artifact, entry.ModulePath); err != nil {
			return err
		}
	}
	for _, entry := range verifier.definitionIndex.Computers {
		if err := artifact.VerifyDeclarationModule(verifier.artifact, entry.ModulePath); err != nil {
			return err
		}
	}

	return nil
}

func (verifier *programVerifier) verifyLinks() error {
	for _, entry := range verifier.artifact.Entries() {
		if entry.Kind == artifact.EntrySymlink {
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
	hops := 1 // Include the link whose target is being validated.
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
		if err := safepath.ValidateTreePath(candidate, artifact.ProgramMountPath); err != nil {
			return err
		}
		entry, exists := verifier.artifact.Lookup(candidate)
		if !exists {
			return nil
		}
		if entry.Kind == artifact.EntrySymlink {
			hops++
			if hops > safepath.TreeLinkHops {
				return fmt.Errorf("target exceeds %d symbolic-link hops", safepath.TreeLinkHops)
			}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		if entry.Kind != artifact.EntryDirectory && len(pending) != 0 {
			return nil
		}
		resolved = append(resolved, component)
	}
	return nil
}
