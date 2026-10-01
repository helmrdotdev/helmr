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
	var sdkMachine *firecrackersdk.Machine
	err := withSDKClientTimeouts(initTimeout, func() error {
		var err error
		sdkMachine, err = firecrackersdk.NewMachine(ctx, cfg, opts...)
		return err
	})
	return sdkMachine, err
}
