//go:build !linux

package guestd

import (
	"context"
	"errors"
	"io"
)

func handleBuild(context.Context, io.ReadWriter, uint64) error {
	return errors.New("build guest requires Linux")
}
