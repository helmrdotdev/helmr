//go:build !linux

package verify

import (
	"context"
	"errors"
)

func Qualify(context.Context, string, string) error {
	return errors.New("artifact verifier qualification requires Linux")
}
