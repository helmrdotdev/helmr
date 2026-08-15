# Worker image module

This module builds the Helmr Worker AMI from exact content-addressed host and
runtime artifacts. The image contains the Worker binaries, certified Firecracker guest images with the
pinned guest BuildKit daemon, and the host tools needed for Platform Artifact
acquisition. The Worker host does not run a BuildKit service.

The image also installs the checked-in `templates/prepare-root.sh` as
`/usr/local/sbin/helmr-prepare-root` with mode `0755`. Image Builder verifies its
source digest, and launch user data supplies only the deployment-specific root
volume size before any secret access.

The complete rendered Image Builder component and immutable recipe inputs have
separate SHA-256 definition digests. Components and recipes carry the full
digest in their resource names with fixed incidental version `1.0.0`; the
stable pipeline points at the current recipe. Source revisions remain external
artifact provenance and are not AMI identity.

Managed Runtime, Manager, and standard-toolchain trees are not baked into the
AMI. A build-capable Worker host acquires exact Control Plane-assigned selectors,
publishes immutable trees to the Platform Artifact CAS, and tenant build work
receives only Control Plane-pinned digests.
