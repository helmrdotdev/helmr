# Release security

Helmr releases three independent products:

- the Control container image;
- the Worker AMI, which contains Helmr executables, guest images, and
  acquisition tools but no Node, package-manager, or build-toolchain catalog;
- the signed Platform release, which contains the immutable Runtime harness,
  base toolchain, and closed build-policy input.

The Platform release is built from one source commit by Nix. Its tar archive is
deterministic, its provenance records the archive digest and build-policy
digest, and `cosign sign-blob` binds those bytes to the release workflow.
Production publication runs only for a Platform tag. The branch/manual
`platform-release-dev` job produces the same archive shape for an exact commit
without repository-write or AWS authority.

Control publishes only complete, closed Platform release objects. It validates
the release manifest and every object before writing immutable
content-addressed bytes to the Platform store. The build policy is published
last, so a partial upload is never usable as a complete release. Object
publication does not activate a GC root or mutate a Deployment.

For a Deployment, Control submits an exact Node and package-manager selector to
a trusted host-side acquisition executor on an existing build-capable Worker.
That executor obtains official upstream artifacts, verifies upstream integrity,
builds closed Runtime/Manager/toolchain trees, performs bounded mechanical
conformance checks, and publishes candidates to the Platform store. Control
then independently reads and validates the complete candidates and atomically
pins all Deployment digests. Tenant Build VMs and ordinary build leases receive
digests only and cannot resolve selectors.

The Platform store is versioned, private, KMS-encrypted, and public-access
blocked. Control and Workers may create and read immutable objects but have no
delete authority. Referenced Deployment pins and active GitOps build-policy
manifests are GC roots. The post-smoke reaper will receive separate,
exact-version deletion authority and must recheck roots under the shared
platform-artifact lock before deletion.

Control images and Worker AMIs are built directly from the same checked-out
source. Their provenance binds the image digest or AMI/Image Builder result to
the exact source commit. They do not embed, download, or validate a mutable
Platform release during image construction.

Release repair never rebuilds signed bytes. It downloads the existing archive,
provenance, and Sigstore bundle and verifies the exact archive with
`cosign verify-blob`.
