//go:build !linux

package worker

import "errors"

func ProveBuildStorage(BuildStorageConfig) (BuildStorageProof, error) {
	return BuildStorageProof{}, errors.New("build storage proof requires Linux")
}
