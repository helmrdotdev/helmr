//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func LoadToolCorpus(
	context.Context,
	*ToolRegistry,
	RuntimeArchitecture,
) (*ToolCorpus, error) {
	return nil, errors.New("dependency tool corpus is supported only on Linux")
}

func (c *ToolCorpus) OpenToolset(
	context.Context,
	Toolset,
) (*ToolObjectFile, error) {
	return nil, errors.New("dependency tool corpus is supported only on Linux")
}
