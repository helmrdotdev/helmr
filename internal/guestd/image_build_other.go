//go:build !linux

package guestd

import (
	"context"
	"errors"
	"io"
)

func handleImageBuild(context.Context, io.ReadWriter, uint64) error {
	return errors.New("image-build guest requires Linux")
}
