# Release security

Release workflows are treated as privileged code because they can publish files, container images, signed boot artifacts, and future AWS worker AMIs.

## Required repository settings

- Create a GitHub Actions environment named `release-production`.
- Restrict deployments to release tags or `main` workflow dispatch runs.
- For a single-maintainer project, leave required reviewers disabled so the maintainer can publish releases.
- When more than one maintainer can approve releases, require reviewer approval for `release-production` and disable self-approval.
- Treat prerelease tags such as `vX.Y.Z-rc.N` as release jobs: they still publish public
  artifacts, use the same protected environment, and must be marked as prereleases instead of
  latest releases.
- Add environment variables for official worker AMI releases:
  - `RELEASE_AWS_ROLE_ARN`: IAM role assumed by GitHub OIDC.
  - `RELEASE_AWS_STATE_BUCKET`: S3 backend bucket for the worker-image OpenTofu state.
  - `RELEASE_AWS_STATE_KEY`: S3 backend key for the release worker-image stack,
    initially `helmr/stacks/release-worker-image/terraform.tfstate`.
  - `RELEASE_AWS_ARTIFACT_BUCKET`: versioned private S3 bucket used to stage the exact
    authenticated Worker runtime-release package for EC2 Image Builder.
  - `RELEASE_AWS_REGION`: primary Image Builder region, initially `us-east-1`.
  - `RELEASE_AWS_STATE_REGION`: state bucket region, if different from `RELEASE_AWS_REGION`.
  - `RELEASE_WORKER_AMI_REGIONS`: comma-separated public AMI regions, initially `us-east-1,us-west-2,ap-northeast-1`.
  - `RELEASE_WORKER_AMI_KEEP`: public release AMIs to keep per region before building the next
    release AMI, initially `4` so the default AWS public AMI quota has one free slot.

## Workflow rules

- Do not use `pull_request_target`.
- Do not use GitHub Actions cache in release workflows.
- Do not pass `CACHIX_AUTH_TOKEN` to release workflows.
- Keep write credentials in the smallest possible job.
- Build jobs should use `contents: read` and upload workflow artifacts.
- The platform-tag runtime producer is the only job that both builds and publishes repository
  output. It needs that boundary to serialize lineage validation, keyless signing, and create-only
  publication as one operation.
- Other publish jobs should download artifacts, check out only the exact release tag, and avoid
  building repository code.
- `id-token: write` is only allowed when the line is marked with `security-check: allow-id-token` and the job is protected by the `release-production` environment.

## Managed runtime release rules

`.github/runtime-release.json` is the reviewed lineage decision. Its `release` must equal the
platform tag. `predecessor` is `null` only for the first managed release; every successor names the
immediately preceding complete `runtime-release.tar` by exact tag, SHA-256, and byte length.

Platform-tag producers use the maximum GitHub Actions concurrency queue so up to 100 pending tags are
retained, serialized across tags, and not replaced by a newer pending run. GitHub services that queue
by the time each run begins waiting, not dispatch order. Overflow is cancelled before composition and
publishes nothing; queue order never selects lineage. Before composition, the workflow refuses an
initial release when any complete managed distribution already exists and refuses a successor whose
checked-in predecessor is not the current published lineage head. The mutable GitHub observation can
only deny the descriptor; it cannot select a predecessor.

`runtime-release.tar` and `runtime-release-x86_64.tar` are fixed create-only release assets. A tag
rerun verifies and consumes an existing complete distribution and byte-compares any existing
Worker package. It never creates a second keyless signature for the same tag. A
`workflow_dispatch` run checks out the requested existing tag, downloads and verifies its complete
distribution, and derives the deterministic Worker package for image repair. It has no OIDC signing
permission and cannot compose or publish a managed runtime release. Every archive consumer obtains
the trusted root from the exact release-tag checkout through the Nix `runtimeTrustedRoot` output.
The verifier byte-compares that pinned root with the archive member before using the pinned bytes
for signature verification; an archive cannot nominate its own trust root.

The verified release feeds both deployment targets:

- the Control image receives the runtime and standard-toolchain
  `catalog.json`, `catalog.sigstore.json`, and `trusted-root.json` files,
  installed root-owned and read-only, but no physical runtime or toolchain
  objects;
- the x86_64 Worker package contains the standard-toolchain closure objects derived from the
  authenticated catalog for that architecture under
  `toolchain-release/objects/sha256/<digest>`. The catalog is the only serving-time manifest:
  producer registries, composed toolsets, and Manager objects are not shipped. The verifier creates
  a read-only snapshot from the exact package bytes it authenticated. Staging exact-checks that
  snapshot against the verifier's SHA-256 and byte length, conditionally creates it in the versioned
  private bucket, downloads that exact version, and checks the bytes again before Image Builder can
  consume it.

Every complete distribution retains the exact deduplicated closure set named by its append-only
standard-toolchain catalog, including predecessor closures. A Worker package contains the exact
matching-architecture subset. Composition, package verification, installation, and Worker startup
derive their expected object sets from the authenticated catalog and reject missing, extra,
wrong-architecture, size-divergent, or digest-divergent objects. No second corpus manifest or
historical download participates in this guarantee.

## Worker AMI release rules

The worker AMI release job assumes a narrowly scoped AWS role through GitHub OIDC and starts or
monitors AWS Image Builder. The publish job assumes the same role to verify that every AMI recorded
in `aws-artifacts.json` is visible in its declared region before publishing the manifest. Do not add
long-lived AWS access keys to GitHub. The actual worker image build happens inside AWS Image
Builder with a separate least-privilege instance profile. A manual repair always rebuilds the AMIs
from the authenticated Worker package. The workflow does not accept bare AMI IDs because they do
not prove which package was used; any future reuse path must consume a closed provenance-bound
artifact rather than reconstructing one from identifiers.

Scope the role trust policy to the repository and `release-production` environment, with
`token.actions.githubusercontent.com:aud` equal to `sts.amazonaws.com` and
`token.actions.githubusercontent.com:sub` equal to:

```text
repo:helmrdotdev/helmr:environment:release-production
```

Set the role maximum session duration to at least four hours so the workflow can poll long Image
Builder runs. The role permissions should cover only the worker-image OpenTofu stack and release
manifest verification: S3 state access, EC2 Image Builder pipeline/configuration resources, the
image-builder instance profile and role, required EC2 describe/distribution calls including
`ec2:DescribeImages`, public release AMI retention cleanup with `ec2:DeregisterImage` and
`ec2:DeleteSnapshot`, and `iam:PassRole` for the image-builder instance profile role.

The staging bucket must have versioning enabled. Scope the release role to create and read only
`helmr/runtime-release-packages/*`, and scope the Image Builder instance role to
`s3:GetObjectVersion` for the one object ARN injected into its immutable recipe. If the bucket uses
SSE-KMS, grant only the corresponding encrypt/decrypt and data-key operations on that key. The
workflow records the exact package URI, object version ID, SHA-256, and optional KMS key identity in
a closed Worker artifact. The same object identity configures the immutable Image Builder recipe
and is copied into `aws-artifacts.json` alongside the resulting AMIs. Neither role nor a later
publish step may resolve a mutable `latest` object or reconstruct the package identity from a key.
