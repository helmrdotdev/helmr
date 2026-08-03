//go:build !linux

package firecracker

import "errors"

const BootCorpusMaxMiB = int64(2048)

func PrepareRuntime(string, string, string) (string, error) {
	return "", errors.New("runtime staging requires linux")
}

func CleanRuntimes(string, string) error {
	return errors.New("runtime staging requires linux")
}
