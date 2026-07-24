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

func handleBuildVerification(context.Context, io.ReadWriter, uint64) error {
	return errors.New("build verification guest requires Linux")
}
