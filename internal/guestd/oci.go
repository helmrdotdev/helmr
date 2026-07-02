package guestd

import (
	"io"

	"github.com/helmrdotdev/helmr/internal/oci"
)

type ociImage = oci.Image
type ociRuntimeConfig = oci.RuntimeConfig
type ociIndex = oci.Index
type ociManifest = oci.Manifest
type ociDescriptor = oci.Descriptor

func unpackOCIImage(r io.Reader, destination string) (ociImage, error) {
	return oci.Unpack(r, destination)
}

func applyLayerTar(r io.Reader, destination string) error {
	return oci.ApplyLayerTar(r, destination)
}

func confinedLayerPath(root string, relative string) (string, error) {
	return oci.ConfinedLayerPath(root, relative)
}
