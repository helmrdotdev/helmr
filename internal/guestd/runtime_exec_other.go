//go:build !linux

package guestd

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

type adapterCommandOptions struct {
	ImageMode bool
	Pty       bool
}

func adapterCommand(ctx context.Context, bunPath string, args []string, launchCwd string, env []string, imageRoot string, user *resolvedRuntimeUser, opts adapterCommandOptions) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bunPath, args...)
	cmd.Dir = launchCwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !opts.Pty}
	if !opts.ImageMode {
		return cmd, nil
	}
	if user == nil {
		return nil, errors.New("image runtime user is required")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: !opts.Pty,
		Chroot:  imageRoot,
		Credential: &syscall.Credential{
			Uid:    user.UID,
			Gid:    user.GID,
			Groups: []uint32{},
		},
	}
	return cmd, nil
}
