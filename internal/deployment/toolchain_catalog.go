package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var errToolchainCatalogUnauthenticated = errors.New("authenticated standard-toolchain catalog is required")

const (
	ToolchainCatalogFormatVersion = 0
	ToolchainCatalogMediaType     = "application/vnd.helmr.standard-toolchain-catalog.v0+json"
	maxToolchainCatalogBytes      = maxProgramFileSizeBytes
)

type ToolchainCatalog struct {
	toolchains      []Toolchain
	toolchainsBytes []byte
	digest          string
	authenticated   bool
}

type toolchainCatalogDocument struct {
	FormatVersion int         `json:"formatVersion"`
	Toolchains    []Toolchain `json:"toolchains"`
}

func ParseToolchainCatalog(raw []byte) (*ToolchainCatalog, error) {
	if len(raw) == 0 || int64(len(raw)) > maxToolchainCatalogBytes {
		return nil, fmt.Errorf(
			"standard-toolchain catalog size is outside [1,%d]",
			maxToolchainCatalogBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize standard-toolchain catalog: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("standard-toolchain catalog is not RFC 8785 canonical JSON")
	}

	var document toolchainCatalogDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode standard-toolchain catalog: %w", err)
	}
	if err := ensureEOF(decoder, "standard-toolchain catalog"); err != nil {
		return nil, err
	}
	if err := validateToolchainCatalogDocument(document); err != nil {
		return nil, err
	}
	complete, err := canonicalToolchainCatalogDocument(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, errors.New("standard-toolchain catalog does not match the complete canonical v0 shape")
	}
	toolchainsBytes, err := canonicalToolchains(document.Toolchains)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	return &ToolchainCatalog{
		toolchains:      append([]Toolchain(nil), document.Toolchains...),
		toolchainsBytes: toolchainsBytes,
		digest:          fmt.Sprintf("sha256:%x", hash[:]),
	}, nil
}

func CanonicalToolchainCatalog(toolchains []Toolchain) ([]byte, error) {
	document := toolchainCatalogDocument{
		FormatVersion: ToolchainCatalogFormatVersion,
		Toolchains:    append([]Toolchain(nil), toolchains...),
	}
	if err := validateToolchainCatalogDocument(document); err != nil {
		return nil, err
	}
	return canonicalToolchainCatalogDocument(document)
}

func toolchainCatalogDigest(toolchains []Toolchain) (string, error) {
	raw, err := CanonicalToolchainCatalog(toolchains)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", hash[:]), nil
}

func (c *ToolchainCatalog) Resolve(digest string) (Toolchain, error) {
	if c == nil || !c.authenticated {
		return Toolchain{}, errToolchainCatalogUnauthenticated
	}
	for _, toolchain := range c.toolchains {
		registered, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return Toolchain{}, err
		}
		if registered == digest {
			return toolchain, nil
		}
	}
	return Toolchain{}, fmt.Errorf("standard toolchain %q is not registered", digest)
}

func (c *ToolchainCatalog) Digest() (string, error) {
	if c == nil || !c.authenticated || !validToolDigest(c.digest) {
		return "", errToolchainCatalogUnauthenticated
	}
	return c.digest, nil
}

func validateToolchainCatalogDocument(document toolchainCatalogDocument) error {
	if document.FormatVersion != ToolchainCatalogFormatVersion {
		return fmt.Errorf(
			"standard-toolchain catalog formatVersion = %d, want %d",
			document.FormatVersion,
			ToolchainCatalogFormatVersion,
		)
	}
	return validateToolchains("standard-toolchain catalog", document.Toolchains)
}

func validateToolchains(label string, toolchains []Toolchain) error {
	if len(toolchains) == 0 || len(toolchains) > maxToolchainCatalogMembers {
		return fmt.Errorf(
			"%s toolchains are outside [1,%d]",
			label,
			maxToolchainCatalogMembers,
		)
	}
	previous := ""
	objects := make([]ToolObject, 0, len(toolchains))
	for position, toolchain := range toolchains {
		if err := validateToolchain(toolchain); err != nil {
			return fmt.Errorf("%s toolchain %d: %w", label, position, err)
		}
		digest, err := StandardToolchainDigest(toolchain)
		if err != nil {
			return fmt.Errorf("%s toolchain %d: %w", label, position, err)
		}
		if position > 0 && previous >= digest {
			return fmt.Errorf(
				"%s toolchains are not in standard-toolchain digest order at position %d",
				label,
				position,
			)
		}
		previous = digest
		objects = append(objects, toolObject(toolchain.ToolchainClosure))
	}
	if _, err := normalizeToolObjects(objects); err != nil {
		return fmt.Errorf("%s physical closures: %w", label, err)
	}
	return nil
}

func canonicalToolchainCatalogDocument(document toolchainCatalogDocument) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode standard-toolchain catalog: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize standard-toolchain catalog: %w", err)
	}
	if len(canonical) == 0 || int64(len(canonical)) > maxToolchainCatalogBytes {
		return nil, fmt.Errorf(
			"standard-toolchain catalog size is outside [1,%d]",
			maxToolchainCatalogBytes,
		)
	}
	return canonical, nil
}

func canonicalToolchains(toolchains []Toolchain) ([]byte, error) {
	raw, err := json.Marshal(toolchains)
	if err != nil {
		return nil, fmt.Errorf("encode standard toolchains: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize standard toolchains: %w", err)
	}
	return canonical, nil
}
