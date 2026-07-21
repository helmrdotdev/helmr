//go:build !linux

package guestd

import "errors"

func createProgramCgroup() (programCgroup, error) {
	return nil, errors.New("managed Program cgroup requires Linux")
}
