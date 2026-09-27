//go:build !linux

package guestd

import "errors"

func createProcessCgroup(string) (processCgroup, error) {
	return nil, errors.New("managed program cgroup requires Linux")
}
