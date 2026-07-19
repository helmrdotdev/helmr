//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func LoadToolchainCorpus(
	context.Context,
	*ToolchainCatalog,
	RuntimeArchitecture,
) (*ToolchainCorpus, error) {
	return nil, errors.New("standard-toolchain corpus is supported only on Linux")
}

func (c *ToolchainCorpus) OpenToolchain(
	context.Context,
	Toolchain,
) (*ToolObjectFile, error) {
	return nil, errors.New("standard-toolchain corpus is supported only on Linux")
}
