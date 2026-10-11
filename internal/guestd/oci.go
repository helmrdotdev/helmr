package guestd

import "github.com/helmrdotdev/helmr/internal/oci"

type ociImage = oci.Image
type ociRuntimeConfig = oci.RuntimeConfig

func confinedLayerPath(root string, relative string) (string, error) {
	return oci.ConfinedLayerPath(root, relative)
}
