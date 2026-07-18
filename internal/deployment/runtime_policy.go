package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	regionpkg "github.com/helmrdotdev/helmr/internal/region"
)

const (
	RuntimePolicyFormatVersion = 0
	maxRuntimePolicyBytes      = maxProgramFileSizeBytes
)

var (
	ErrRuntimeRegionNotConfigured = errors.New("runtime policy region is not configured")
	ErrRuntimeNotRegistered       = errors.New("runtime is not registered")
)

type RuntimePolicy struct {
	current       map[string]string
	runtimes      map[string]RuntimeDescriptor
	runtimesBytes []byte
}

type runtimePolicyDocument struct {
	Current       map[string]string   `json:"current"`
	FormatVersion int                 `json:"formatVersion"`
	Runtimes      []RuntimeDescriptor `json:"runtimes"`
}

func LoadRuntimePolicy(path string, catalog *RuntimeCatalog) (*RuntimePolicy, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open runtime policy: %w", err)
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxRuntimePolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read runtime policy: %w", err)
	}
	policy, err := ParseRuntimePolicy(raw)
	if err != nil {
		return nil, err
	}
	if err := validateRuntimePolicyCatalog(policy, catalog); err != nil {
		return nil, err
	}
	return policy, nil
}

// ParseRuntimePolicy validates document shape only. Operational callers must
// use LoadRuntimePolicy to bind the policy to an authenticated runtime catalog.
func ParseRuntimePolicy(raw []byte) (*RuntimePolicy, error) {
	if len(raw) == 0 || int64(len(raw)) > maxRuntimePolicyBytes {
		return nil, fmt.Errorf(
			"runtime policy size is outside [1,%d]",
			maxRuntimePolicyBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime policy: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("runtime policy is not RFC 8785 canonical JSON")
	}

	var document runtimePolicyDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode runtime policy: %w", err)
	}
	if err := ensureEOF(decoder, "runtime policy"); err != nil {
		return nil, err
	}
	if err := validateRuntimePolicyDocument(document); err != nil {
		return nil, err
	}

	complete, err := canonicalRuntimePolicyDocument(document)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, complete) {
		return nil, fmt.Errorf("runtime policy does not match the complete canonical v0 shape")
	}
	runtimesBytes, err := canonicalRuntimeDescriptors(document.Runtimes)
	if err != nil {
		return nil, err
	}

	policy := &RuntimePolicy{
		current:       make(map[string]string, len(document.Current)),
		runtimes:      make(map[string]RuntimeDescriptor, len(document.Runtimes)),
		runtimesBytes: runtimesBytes,
	}
	for region, digest := range document.Current {
		policy.current[region] = digest
	}
	for _, descriptor := range document.Runtimes {
		policy.runtimes[descriptor.Digest] = descriptor
	}
	return policy, nil
}

func validateRuntimePolicyCatalog(policy *RuntimePolicy, catalog *RuntimeCatalog) error {
	if policy == nil {
		return errors.New("runtime policy is required")
	}
	if catalog == nil || !catalog.authenticated {
		return errors.New("authenticated runtime catalog is required")
	}
	if !bytes.Equal(policy.runtimesBytes, catalog.runtimesBytes) {
		return errors.New("runtime policy runtimes do not exact-match authenticated catalog")
	}
	return nil
}

func (p *RuntimePolicy) Current(region string) (RuntimeDescriptor, error) {
	if p == nil {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeRegionNotConfigured, region)
	}
	digest, ok := p.current[region]
	if !ok {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeRegionNotConfigured, region)
	}
	return p.runtimes[digest], nil
}

func (p *RuntimePolicy) Resolve(digest string) (RuntimeDescriptor, error) {
	if p == nil {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeNotRegistered, digest)
	}
	descriptor, ok := p.runtimes[digest]
	if !ok {
		return RuntimeDescriptor{}, fmt.Errorf("%w: %q", ErrRuntimeNotRegistered, digest)
	}
	return descriptor, nil
}

func ValidateRuntimePolicyUpgrade(previous, next *RuntimePolicy) error {
	if previous == nil || next == nil {
		return errors.New("runtime policy upgrade requires both snapshots")
	}
	for digest, descriptor := range previous.runtimes {
		replacement, ok := next.runtimes[digest]
		if !ok {
			return fmt.Errorf("runtime policy upgrade removes registered digest %q", digest)
		}
		if replacement != descriptor {
			return fmt.Errorf("runtime policy upgrade mutates registered digest %q", digest)
		}
	}
	return nil
}

func validateRuntimePolicyDocument(document runtimePolicyDocument) error {
	if document.FormatVersion != RuntimePolicyFormatVersion {
		return fmt.Errorf(
			"runtime policy formatVersion = %d, want %d",
			document.FormatVersion,
			RuntimePolicyFormatVersion,
		)
	}
	if document.Current == nil {
		return fmt.Errorf("runtime policy current must be an object")
	}
	registered := make(map[string]struct{}, len(document.Runtimes))
	if err := validateRuntimeDescriptors("runtime policy", document.Runtimes); err != nil {
		return err
	}
	for _, descriptor := range document.Runtimes {
		registered[descriptor.Digest] = struct{}{}
	}
	for region, digest := range document.Current {
		if err := regionpkg.ValidateID(region); err != nil {
			return fmt.Errorf("runtime policy current region %q: %w", region, err)
		}
		if _, ok := registered[digest]; !ok {
			return fmt.Errorf(
				"runtime policy current region %q references unregistered digest %q",
				region,
				digest,
			)
		}
	}
	return nil
}

func canonicalRuntimePolicyDocument(document runtimePolicyDocument) ([]byte, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode runtime policy: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime policy: %w", err)
	}
	if int64(len(canonical)) > maxRuntimePolicyBytes {
		return nil, fmt.Errorf(
			"runtime policy size is outside [1,%d]",
			maxRuntimePolicyBytes,
		)
	}
	return canonical, nil
}
