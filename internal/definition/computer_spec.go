package definition

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/oci"
)

const (
	computerSpecDigestDomain = "helmr.computer-spec.v0\x00"

	RuntimeContract            = "helmr.runtime.v0"
	ArchitectureX8664          = RuntimeArchitecture("x86_64")
	MaxComputerSeedBytes int64 = 17179869184

	// ComputerSeedProfile and ComputerSeedMediaType identify the client-built
	// Computer seed disk format.
	ComputerSeedProfile   = "linux-amd64-ext4-v1"
	ComputerSeedMediaType = "application/vnd.helmr.computer.seed.v0+filepack"
)

type RuntimeArchitecture string

// ComputerSeed is the verified seed image used by a Computer definition.
type ComputerSeed struct {
	Profile      string
	Config       oci.RuntimeConfig
	Architecture RuntimeArchitecture
	Digest       string
	MediaType    string
	SizeBytes    int64
}

// ComputerConfig contains the immutable launch requirements of a Computer.
// Program content, deployment provenance and Secret bindings are separate owners.
// Profile identifies the seed filesystem and its mount contract.
type ComputerConfig struct {
	Architecture    RuntimeArchitecture `json:"architecture"`
	RuntimeContract string              `json:"runtimeContract"`
	Profile         string              `json:"profile"`
	Image           oci.RuntimeConfig   `json:"image"`
	Resources       ResourcesManifest   `json:"resources"`
}

type ComputerSpec struct {
	Config []byte
	Seed   cas.Descriptor
	Digest [sha256.Size]byte
}

// ParseComputerSpec validates stored JSON, then normalizes before hashing. It
// therefore has the same identity after a JSONB round trip without accepting
// duplicate keys or silently discarding unknown configuration.
func ParseComputerSpec(raw []byte, seed cas.Descriptor) (ComputerSpec, error) {
	config, err := ParseComputerConfig(raw)
	if err != nil {
		return ComputerSpec{}, err
	}
	if err := cas.ValidateDescriptor(seed); err != nil {
		return ComputerSpec{}, fmt.Errorf("computer seed: %w", err)
	}
	if seed.MediaType != ComputerSeedMediaType || seed.SizeBytes > MaxComputerSeedBytes {
		return ComputerSpec{}, errors.New("unsupported computer seed descriptor")
	}
	normalized, err := json.Marshal(config)
	if err != nil {
		return ComputerSpec{}, err
	}
	canonical, err := jsoncanon.Transform(normalized)
	if err != nil {
		return ComputerSpec{}, err
	}
	identity, err := json.Marshal(struct {
		Config json.RawMessage `json:"config"`
		Seed   seedObject      `json:"seed"`
	}{Config: canonical, Seed: seedObject{Digest: seed.Digest, SizeBytes: seed.SizeBytes, MediaType: seed.MediaType}})
	if err != nil {
		return ComputerSpec{}, err
	}
	identity, err = jsoncanon.Transform(identity)
	if err != nil {
		return ComputerSpec{}, err
	}
	return ComputerSpec{Config: canonical, Seed: seed, Digest: domainDigest(computerSpecDigestDomain, identity)}, nil
}

// ParseComputerConfig validates the immutable launch requirements independently of
// seed materialization. Dispatch consumes these requirements directly.
func ParseComputerConfig(raw []byte) (ComputerConfig, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ComputerConfig{}, fmt.Errorf("canonicalize computer config: %w", err)
	}
	var config ComputerConfig
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return ComputerConfig{}, fmt.Errorf("decode computer config: %w", err)
	}
	if config.Architecture != ArchitectureX8664 || config.RuntimeContract != RuntimeContract || config.Profile != ComputerSeedProfile {
		return ComputerConfig{}, errors.New("unsupported computer launch contract")
	}
	if err := ValidateResourcesManifest(config.Resources); err != nil {
		return ComputerConfig{}, fmt.Errorf("computer resources: %w", err)
	}
	// Empty arrays and absent OCI defaults have the same launch meaning. Preserve
	// ordered environment entries, entrypoint and arguments, including duplicates.
	config.Image.Env = append([]string{}, config.Image.Env...)
	config.Image.Entrypoint = append([]string{}, config.Image.Entrypoint...)
	config.Image.Cmd = append([]string{}, config.Image.Cmd...)
	return config, nil
}

type seedObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}
