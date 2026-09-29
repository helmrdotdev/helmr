package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/safepath"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const ProgramManifestFormatVersion = 0

// ProgramManifest binds the executable payload and its declaration index.
type ProgramManifest struct {
	FormatVersion      int               `json:"formatVersion"`
	Config             ProgramPathDigest `json:"config"`
	PayloadDigest      string            `json:"payloadDigest"`
	ProgramIndexDigest string            `json:"programIndexDigest"`
}
type ProgramPathDigest struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
}

func ParseProgramManifest(raw []byte) (ProgramManifest, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return ProgramManifest{}, fmt.Errorf(
			"program manifest size is outside [1,%d]",
			MaxProgramFileSizeBytes,
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
	complete, err := CanonicalProgramManifest(manifest)
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

func CanonicalProgramManifest(manifest ProgramManifest) ([]byte, error) {
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
		!sha256DigestPattern.MatchString(value.Config.Digest) || !sha256DigestPattern.MatchString(value.PayloadDigest) || !sha256DigestPattern.MatchString(value.ProgramIndexDigest) {
		return errors.New("program manifest v0 authority is invalid")
	}
	return nil
}
func ProgramManifestFromCompilerResult(value ProgramCompilerResult, indexDigest string) ProgramManifest {
	return ProgramManifest{FormatVersion: ProgramManifestFormatVersion, Config: value.Config, PayloadDigest: value.PayloadDigest, ProgramIndexDigest: indexDigest}
}
func VerifyProgramManifestFiles(ctx context.Context, artifact *Tree, value ProgramManifest) error {
	if err := VerifyProgramPathDigest(ctx, artifact, value.Config); err != nil {
		return err
	}
	raw, err := artifact.Read(ctx, value.Config.Path, maxBuildConfigBytes)
	if err != nil {
		return err
	}
	if _, err := ParseBuildConfig(raw); err != nil {
		return err
	}
	digest, err := artifactPayloadDigest(ctx, artifact)
	if err != nil {
		return err
	}
	if digest != value.PayloadDigest {
		return errors.New("program input tree digest does not match authority")
	}
	entry, canonical, err := resolveProgramArtifactPath(artifact, "package.json")
	if err != nil {
		return err
	}
	if entry.Kind != EntryRegular {
		return errors.New("root package.json must be a contained regular object")
	}
	raw, err = artifact.Read(ctx, canonical, MaxProgramFileSizeBytes)
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return errors.New("root package.json must contain a JSON object")
	}
	return nil
}
func VerifyDeclarationModule(artifact *Tree, value string) error {
	if err := validateDeclarationModulePath(value); err != nil {
		return err
	}
	entry, canonical, err := resolveProgramArtifactPath(artifact, value)
	if err != nil {
		return err
	}
	if entry.Kind != EntryRegular || canonical != value {
		return errors.New("declaration source must be a canonical regular file")
	}
	return nil
}
func resolveProgramArtifactPath(
	artifact *Tree,
	value string,
) (Entry, string, error) {
	if err := ValidatePath(value, RoleProgram); err != nil || value == "." {
		return Entry{}, "", fmt.Errorf("program path %q is invalid", value)
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
				return Entry{}, "", fmt.Errorf("program path %q escapes the artifact", value)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		entry, exists := artifact.entries[candidate]
		if !exists {
			return Entry{}, "", fmt.Errorf("program path %q is missing", candidate)
		}
		if entry.Kind == EntrySymlink {
			hops++
			if hops > safepath.TreeLinkHops {
				return Entry{}, "", fmt.Errorf(
					"program path %q exceeds %d symbolic-link hops",
					value,
					safepath.TreeLinkHops,
				)
			}
			state := candidate + "\x00" + strings.Join(pending, "\x00")
			if _, exists := visited[state]; exists {
				return Entry{}, "", fmt.Errorf("program path %q contains a link cycle", value)
			}
			visited[state] = struct{}{}
			pending = append(strings.Split(entry.LinkTarget, "/"), pending...)
			continue
		}
		resolved = candidateParts
		if len(pending) != 0 && entry.Kind != EntryDirectory {
			return Entry{}, "", fmt.Errorf(
				"program path %q traverses non-directory %q",
				value,
				candidate,
			)
		}
		if len(pending) == 0 {
			return entry, candidate, nil
		}
	}
	return Entry{}, "", fmt.Errorf("program path %q is empty", value)
}

func ProgramIndexDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return sha256sum.FormatDigest(digest[:])
}
