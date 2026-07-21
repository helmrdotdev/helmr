package guestd

import "os/exec"

type programCgroup interface {
	attach(*exec.Cmd) error
	kill() error
	waitEmpty() error
	close() error
}
