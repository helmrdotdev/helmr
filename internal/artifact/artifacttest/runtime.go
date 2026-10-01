package artifacttest

import (
	"strings"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

// RuntimeDescriptor describes a minimal x86_64 runtime artifact.
func RuntimeDescriptor() artifact.RuntimeDescriptor {
	return artifact.RuntimeDescriptor{
		Architecture:    definition.ArchitectureX8664,
		Digest:          "sha256:" + strings.Repeat("a", 64),
		FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
		MediaType:       artifact.RuntimeArtifactMediaType,
		RuntimeContract: definition.RuntimeContract,
		SizeBytes:       artifact.SquashFSPhysicalAlign,
	}
}

// NodeProgramFlags are the Node program flags of the supported Node version.
func NodeProgramFlags() []string {
	flags, _ := artifact.NodeProgramFlags("24.21.0")
	return flags
}
