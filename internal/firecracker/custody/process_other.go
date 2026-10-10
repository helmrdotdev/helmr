//go:build !linux

package custody

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func Stop(context.Context, StateRoot, string, vm.Owner, int) error {
	return errors.New("owned VM process recovery requires Linux pidfds")
}
