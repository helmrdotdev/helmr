package deployment

// These limits remain while the local builder still uses the generic artifact
// encoder for its intermediate manager and build trees. They are not Control
// Plane admission contracts and are never present in deployment bundles.
const (
	ManagerTreeMediaType       = "application/vnd.helmr.package-manager.v0+squashfs"
	maxManagerTreeBytes  int64 = 512 << 20
)
