# Worker image module

This module builds the Helmr Worker AMI from one exact source ref. The image
contains the Worker binaries, certified Firecracker guest images with the
pinned guest BuildKit daemon, and the host tools needed for Platform Artifact
acquisition. The Worker host does not run a BuildKit service.

The image also installs the checked-in `templates/prepare-root.sh` as
`/usr/local/sbin/helmr-prepare-root` with mode `0755`. Image Builder verifies its
source digest, and launch user data supplies only the deployment-specific root
volume size before any secret access.

Managed Runtime, Manager, and standard-toolchain trees are not baked into the
AMI. A build-capable Worker host acquires exact Control Plane-assigned selectors,
publishes immutable trees to the Platform Artifact CAS, and tenant build work
receives only Control Plane-pinned digests.
