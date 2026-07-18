package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var errRuntimeCatalogUnauthenticated = errors.New("authenticated runtime catalog is required")

const (
	RuntimeCatalogFormatVersion = 0
	RuntimeCatalogMediaType     = "application/vnd.helmr.runtime-catalog.v0+json"
	maxRuntimeCatalogBytes      = maxProgramFileSizeBytes
)

type RuntimeCatalog struct {
	runtimes      []RuntimeDescriptor
	runtimesBytes []byte
	authenticated bool
}

type runtimeCatalogDocument struct {
	FormatVersion int                 `json:"formatVersion"`
	Runtimes      []RuntimeDescriptor `json:"runtimes"`
}

func ParseRuntimeCatalog(raw []byte) (*RuntimeCatalog, error) {
	if len(raw) == 0 || int64(len(raw)) > maxRuntimeCatalogBytes {
		return nil, fmt.Errorf(
			"runtime catalog size is outside [1,%d]",
			maxRuntimeCatalogBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime catalog: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("runtime catalog is not RFC 8785 canonical JSON")
	}

	var document runtimeCatalogDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode runtime catalog: %w", err)
	}
	if err := ensureEOF(decoder, "runtime catalog"); err != nil {
		return nil, err
	}
	if err := validateRuntimeCatalogDocument(document); err != nil {
		return nil, err
	}

	complete, err := canonicalRuntimeCatalogDocument(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, fmt.Errorf("runtime catalog does not match the complete canonical v0 shape")
	}
	runtimesBytes, err := canonicalRuntimeDescriptors(document.Runtimes)
	if err != nil {
		return nil, err
	}

	return &RuntimeCatalog{
		runtimes:      append([]RuntimeDescriptor(nil), document.Runtimes...),
		runtimesBytes: runtimesBytes,
	}, nil
}

func CanonicalRuntimeCatalog(runtimes []RuntimeDescriptor) ([]byte, error) {
	document := runtimeCatalogDocument{
		FormatVersion: RuntimeCatalogFormatVersion,
		Runtimes:      append([]RuntimeDescriptor(nil), runtimes...),
	}
	if err := validateRuntimeCatalogDocument(document); err != nil {
		return nil, err
	}
	return canonicalRuntimeCatalogDocument(document)
}

func (c *RuntimeCatalog) Resolve(digest string) (RuntimeDescriptor, error) {
	if c == nil || !c.authenticated {
		return RuntimeDescriptor{}, errRuntimeCatalogUnauthenticated
	}
	for _, descriptor := range c.runtimes {
		if descriptor.Digest == digest {
			return descriptor, nil
		}
	}
	return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeNotRegistered, digest)
}

func validateRuntimeCatalogDocument(document runtimeCatalogDocument) error {
	if document.FormatVersion != RuntimeCatalogFormatVersion {
		return fmt.Errorf(
			"runtime catalog formatVersion = %d, want %d",
			document.FormatVersion,
			RuntimeCatalogFormatVersion,
		)
	}
	return validateRuntimeDescriptors("runtime catalog", document.Runtimes)
}

func validateRuntimeDescriptors(label string, runtimes []RuntimeDescriptor) error {
	if len(runtimes) == 0 {
		return fmt.Errorf("%s runtimes must be a non-empty array", label)
	}
	for position, descriptor := range runtimes {
		if err := ValidateRuntimeDescriptor(descriptor); err != nil {
			return fmt.Errorf("%s runtime %d: %w", label, position, err)
		}
		if position > 0 && runtimes[position-1].Digest >= descriptor.Digest {
			return fmt.Errorf(
				"%s runtimes are not in digest order at position %d",
				label,
				position,
			)
		}
	}
	return nil
}

func canonicalRuntimeCatalogDocument(document runtimeCatalogDocument) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode runtime catalog: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime catalog: %w", err)
	}
	if len(canonical) == 0 || int64(len(canonical)) > maxRuntimeCatalogBytes {
		return nil, fmt.Errorf(
			"runtime catalog size is outside [1,%d]",
			maxRuntimeCatalogBytes,
		)
	}
	return canonical, nil
}

func canonicalRuntimeDescriptors(runtimes []RuntimeDescriptor) ([]byte, error) {
	raw, err := json.Marshal(runtimes)
	if err != nil {
		return nil, fmt.Errorf("encode runtime descriptors: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime descriptors: %w", err)
	}
	return canonical, nil
}
