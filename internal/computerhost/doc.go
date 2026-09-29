// Package computerhost owns the physical side of a Computer on a worker host:
// the prepared machines, the checkout that releases them, serving each mounted
// Computer Instance (mounts, saves, managed commands, authority renewal and
// guest control), and checkpoint capture, encryption and restore.
//
// Runs reach a mounted Computer only through the borrowed channel and the typed
// operations this package exports for them. The Run side never checks out,
// serves or directly closes machines; it may release a machine only through
// the mount-bound MountChannel.ReleaseSource, as a checkpoint source after its
// stream has detached.
package computerhost
