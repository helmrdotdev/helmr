# Worker image module

This module builds the Helmr Worker AMI from exact content-addressed host and
runtime artifacts. The image contains the Worker binaries, certified
Firecracker guest images, and the host tools required to qualify and execute
the pinned Runtime. Deployment bundles are built by the local/CI builder and
Workers do not run a BuildKit service or acquire Product release artifacts.

The image also installs the checked-in `templates/prepare-root.sh` as
`/usr/local/sbin/helmr-prepare-root` with mode `0755`. Image Builder verifies its
source digest, and launch user data supplies only the deployment-specific root
volume size before any secret access.

The complete rendered Image Builder component and immutable recipe inputs have
separate SHA-256 definition digests. Components and recipes carry the full
digest in their resource names with fixed incidental version `1.0.0`; the
stable pipeline points at the current recipe. Source revisions remain external
artifact provenance and are not AMI identity.

The Product release publishes the exact Runtime object before deployment. A
Worker retrieves that Control Plane-pinned object, validates it, and admits
execution only after the real Firecracker qualification path succeeds.
