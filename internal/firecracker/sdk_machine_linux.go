//go:build linux

package firecracker

import (
	"context"
	"time"

	firecrackersdk "github.com/firecracker-microvm/firecracker-go-sdk"
)

func newSDKMachine(
	ctx context.Context,
	cfg firecrackersdk.Config,
	initTimeout time.Duration,
	opts ...firecrackersdk.Opt,
) (*firecrackersdk.Machine, error) {
	var machine *firecrackersdk.Machine
	err := withSDKInitTimeout(initTimeout, func() error {
		var err error
		machine, err = firecrackersdk.NewMachine(ctx, cfg, opts...)
		return err
	})
	return machine, err
}
