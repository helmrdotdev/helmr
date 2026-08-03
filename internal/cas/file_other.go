//go:build !linux

package cas

import (
	"errors"
	"os"
)

type FileIdentity struct {
	size int64
}

func InspectPublishedFile(*os.File) (FileIdentity, error) {
	return FileIdentity{}, errors.New("descriptor-bound publication requires Linux")
}
