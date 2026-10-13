package definition

import (
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/oci"
)

const (
	RuntimeContract            = "helmr.runtime.v0"
	ArchitectureX8664          = RuntimeArchitecture("x86_64")
	MaxComputerSeedBytes int64 = 17179869184

	// ComputerSeedProfile and ComputerSeedMediaType identify the client-built
	// Computer seed disk format.
	ComputerSeedProfile   = "linux-amd64-ext4-v1"
	ComputerSeedMediaType = "application/vnd.helmr.computer.seed.v0+filepack"
)

type RuntimeArchitecture string

// ComputerSeed is the verified seed image used by a Template definition.
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
