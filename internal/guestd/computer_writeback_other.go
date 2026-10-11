//go:build !linux

package guestd

import "errors"

func openComputerWriteback() (*computerWriteback, error) {
	return nil, errors.New("computer writeback requires Linux")
}
