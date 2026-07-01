//go:build !linux

package guestd

import (
	"errors"
	"io"
	"os"
	"strings"
)

const guestdSubstrateRootEnv = "HELMR_GUESTD_SUBSTRATE_ROOT"

func guestdSubstrateRoot() string {
	return strings.TrimSpace(os.Getenv(guestdSubstrateRootEnv))
}

func imageFromMountedSubstrate(io.Reader, string) (ociImage, func(), error) {
	return ociImage{}, func() {}, errors.New("runtime substrate overlay is only supported on Linux")
}
