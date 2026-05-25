//go:build !linux

package guestd

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func adapterCommand(ctx context.Context, bunPath string, args []string, launchCwd string, env []string, imageRoot string, user *resolvedRuntimeUser, imageMode bool) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, bunPath, args...)
	cmd.Dir = launchCwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !imageMode {
		return cmd, nil
	}
	if user == nil {
		return nil, errors.New("image runtime user is required")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Chroot: imageRoot,
		Credential: &syscall.Credential{
			Uid:    user.UID,
			Gid:    user.GID,
			Groups: []uint32{},
		},
	}
	return cmd, nil
}
