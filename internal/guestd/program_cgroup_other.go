//go:build !linux

package guestd

import "errors"

func createProgramCgroup(string) (programCgroup, error) {
	return nil, errors.New("managed program cgroup requires Linux")
}
