package guestd

import (
	"context"
	"os/exec"
)

type programCgroup interface {
	attach(*exec.Cmd) error
	freeze(context.Context) error
	thaw(context.Context) error
	kill() error
	waitEmpty() error
	close() error
}
