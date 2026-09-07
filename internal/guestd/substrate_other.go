//go:build !linux

package guestd

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/oci"
)

const guestdSubstrateRootEnv = "HELMR_GUESTD_SUBSTRATE_ROOT"

func guestdSubstrateRoot() string {
	return strings.TrimSpace(os.Getenv(guestdSubstrateRootEnv))
}

func imageFromMountedSubstrate(io.Reader, string) (ociImage, func(), error) {
	return ociImage{}, func() {}, errors.New("runtime substrate overlay is only supported on Linux")
}

func imageFromMountedSubstrateConfig(oci.RuntimeConfig, string) (ociImage, func(), error) {
	return ociImage{}, func() {}, errors.New("runtime substrate overlay is only supported on Linux")
}
