# Release security

Helmr releases four independent products:

- the Control Plane container image;
- the execution Worker AMI;
- the signed Platform release containing the immutable Runtime closure used by
  verified Deployment bundles;
- the digest-pinned local/CI bundle-builder image.

The Platform release is built from one source commit by Nix. Its deterministic
archive and Sigstore bundle bind the exact runtime bytes to the release
workflow. The Control Plane publish command accepts only the canonical closed
runtime manifest, validates its descriptor, and writes the object immutably to
the Platform store. It has no package-manager, toolchain-selection, source
build, or activation authority.

The bundle-builder image owns dependency installation and project compilation.
The CLI pins that image by registry digest, runs it through isolated BuildKit,
and uploads only its content-addressed output closure. Build credentials are
producer-local inputs and must not enter the bundle. The Control Plane verifies
the closure, formats, sizes, architecture, and Runtime contract; it never
executes dependency lifecycle scripts or rebuilds an artifact.

The Platform store is versioned, private, KMS-encrypted, and public-access
blocked. Control Plane and Workers may read immutable runtime objects but have
no delete authority. Referenced Deployment runtime digests are GC roots.

Control Plane images and Worker AMIs are built directly from the same checked
out source. Release repair never rebuilds signed bytes: it downloads and
verifies the existing archive, provenance, and Sigstore bundle.
