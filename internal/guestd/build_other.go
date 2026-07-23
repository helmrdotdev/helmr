//go:build !linux

package guestd

import (
	"context"
	"errors"
	"io"
)

func handleBuildInstall(context.Context, io.ReadWriter, uint64) error {
	return errors.New("build install guest requires Linux")
}

func handleBuildAnalysis(context.Context, io.ReadWriter, uint64) error {
	return errors.New("build analysis guest requires Linux")
}

func handleProgramProof(context.Context, io.ReadWriter, uint64) error {
	return errors.New("Program proof guest requires Linux")
}
