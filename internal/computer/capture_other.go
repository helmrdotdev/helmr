//go:build !linux

package computer

import (
	"context"
	"errors"
)

func (s DiskStore) Capture(context.Context, string, string, string) (*DiskCandidate, error) {
	return nil, errors.New("Computer capture requires Linux")
}
