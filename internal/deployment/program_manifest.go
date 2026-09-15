package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const ProgramManifestFormatVersion = 0

// ProgramManifest binds every installed input and the source declaration index.
type ProgramManifest struct {
	FormatVersion      int               `json:"formatVersion"`
	Config             ProgramPathDigest `json:"config"`
	InputTreeDigest    string            `json:"inputTreeDigest"`
	ProgramIndexDigest string            `json:"programIndexDigest"`
}
type ProgramPathDigest struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
}

func ParseProgramManifest(raw []byte) (ProgramManifest, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramManifest{}, fmt.Errorf(
			"program manifest size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramManifest{}, fmt.Errorf("canonicalize program manifest: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramManifest{}, errors.New(
			"program manifest is not RFC 8785 canonical JSON",
		)
	}
	var manifest ProgramManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ProgramManifest{}, fmt.Errorf("decode program manifest: %w", err)
	}
	if err := ensureEOF(decoder, "program manifest"); err != nil {
		return ProgramManifest{}, err
	}
	if err := validateProgramManifest(manifest); err != nil {
		return ProgramManifest{}, err
	}
	complete, err := canonicalProgramManifest(manifest)
	if err != nil {
		return ProgramManifest{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramManifest{}, errors.New(
			"program manifest does not match the complete canonical v0 shape",
		)
	}
	return manifest, nil
}

func canonicalProgramManifest(manifest ProgramManifest) ([]byte, error) {
	if err := validateProgramManifest(manifest); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

func validateProgramManifest(value ProgramManifest) error {
	if value.FormatVersion != ProgramManifestFormatVersion || value.Config.Path != "helmr/config.json" ||
		!sha256DigestPattern.MatchString(value.Config.Digest) || !sha256DigestPattern.MatchString(value.InputTreeDigest) || !sha256DigestPattern.MatchString(value.ProgramIndexDigest) {
		return errors.New("program manifest v0 authority is invalid")
	}
	return nil
}
func programManifestFromCompilerResult(value ProgramCompilerResult, indexDigest string) ProgramManifest {
	return ProgramManifest{FormatVersion: ProgramManifestFormatVersion, Config: value.Config, InputTreeDigest: value.InputTreeDigest, ProgramIndexDigest: indexDigest}
}
func verifyProgramManifestFiles(ctx context.Context, artifact *inspectedArtifact, value ProgramManifest) error {
	if err := verifyProgramPathDigest(ctx, artifact, value.Config); err != nil {
		return err
	}
	raw, err := artifact.read(ctx, value.Config.Path, maxBuildConfigBytes)
	if err != nil {
		return err
	}
	if _, err := ParseBuildConfig(raw); err != nil {
		return err
	}
	digest, err := artifactInputTreeDigest(ctx, artifact)
	if err != nil {
		return err
	}
	if digest != value.InputTreeDigest {
		return errors.New("program input tree digest does not match authority")
	}
	entry, canonical, err := resolveProgramArtifactPath(artifact, "package.json")
	if err != nil {
		return err
	}
	if entry.Kind != artifactEntryRegular {
		return errors.New("root package.json must be a contained regular object")
	}
	raw, err = artifact.read(ctx, canonical, maxProgramFileSizeBytes)
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return errors.New("root package.json must contain a JSON object")
	}
	return nil
}
func verifyDeclarationSource(artifact *inspectedArtifact, value string) error {
	if err := validateDeclarationSourcePath(value); err != nil {
		return err
	}
	entry, canonical, err := resolveProgramArtifactPath(artifact, value)
	if err != nil {
		return err
	}
	if entry.Kind != artifactEntryRegular || canonical != value {
		return errors.New("declaration source must be a canonical regular file")
	}
	if _, exists := artifact.entries["helmr.config.ts"]; exists {
		_, config, err := resolveProgramArtifactPath(artifact, "helmr.config.ts")
		if err != nil {
			return err
		}
		if config == canonical {
			return errors.New("root config is build-only")
		}
	}
	return nil
}
func resolveProgramArtifactPath(
	artifact *inspectedArtifact,
	value string,
) (artifactEntry, string, error) {
	if err := validateArtifactPath(value, programArtifact); err != nil || value == "." {
		return artifactEntry{}, "", fmt.Errorf("program path %q is invalid", value)
	}
	pending := strings.Split(value, "/")
	resolved := make([]string, 0, len(pending))
	visited := make(map[string]struct{})
	hops := 0
	for len(pending) != 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return artifactEntry{}, "", fmt.Errorf("program path %q escapes the artifact", value)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		entry, exists := artifact.entries[candidate]
		if !exists {
			return artifactEntry{}, "", fmt.Errorf("program path %q is missing", candidate)
		}
		if entry.Kind == artifactEntrySymlink {
			hops++
			if hops > maxSymlinkHops {
				return artifactEntry{}, "", fmt.Errorf(
					"program path %q exceeds %d symbolic-link hops",
					value,
					maxSymlinkHops,
				)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return artifactEntry{}, "", fmt.Errorf("program path %q contains a link cycle", value)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != artifactEntryDirectory {
			return artifactEntry{}, "", fmt.Errorf(
				"program path %q traverses non-directory %q",
				value,
				candidate,
			)
		}
		if len(pending) == 0 {
			return entry, candidate, nil
		}
	}
	return artifactEntry{}, "", fmt.Errorf("program path %q is empty", value)
}

func programIndexDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return sha256sum.FormatDigest(digest[:])
}
