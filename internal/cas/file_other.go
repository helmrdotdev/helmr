//go:build !linux

package cas

import (
	"errors"
	"os"
)

type fileIdentity struct {
	size int64
}

func inspectPublishedFile(*os.File) (fileIdentity, error) {
	return fileIdentity{}, errors.New("descriptor-bound publication requires Linux")
}
