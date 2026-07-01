package guestd

import (
	"io"

	"github.com/helmrdotdev/helmr/internal/ociimage"
)

type ociImage = ociimage.Image
type ociRuntimeConfig = ociimage.RuntimeConfig
type ociIndex = ociimage.Index
type ociManifest = ociimage.Manifest
type ociDescriptor = ociimage.Descriptor

func unpackOCIImage(r io.Reader, destination string) (ociImage, error) {
	return ociimage.Unpack(r, destination)
}

func applyLayerTar(r io.Reader, destination string) error {
	return ociimage.ApplyLayerTar(r, destination)
}

func confinedLayerPath(root string, relative string) (string, error) {
	return ociimage.ConfinedLayerPath(root, relative)
}
