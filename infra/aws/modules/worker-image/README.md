# Worker image module

This module builds the Helmr Worker AMI from one exact source ref. The image
contains the Worker binaries, Firecracker guest images, BuildKit, CNI, and the
host tools needed for Platform Artifact acquisition.

Managed Runtime, Manager, and standard-toolchain trees are not baked into the
AMI. A build-capable Worker host acquires exact Control-assigned selectors,
publishes immutable trees to the Platform Artifact CAS, and tenant build work
receives only Control-pinned digests.
