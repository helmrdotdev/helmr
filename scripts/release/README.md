# Product release checkpoints

PRs and main pushes run the same three fixed source checks: workflow/security
policy, Go command compilation and unit tests, and TypeScript types and unit tests.
The `ci complete` gate rejects failure, cancellation or a skipped check. Source CI
never produces distribution artifacts. There is no changed-path classifier or
full-check label. Repository-wide databases, browser acceptance, generated-source
verification and packaging are checked at a release checkpoint instead of blocking
every merge. Supported deeper checks remain available as explicit local Nix apps.

## Create a preview

After a coherent set of changes reaches main, choose the full main commit SHA and
request one checkpoint:

```sh
gh workflow run release.yaml --ref main -f commit=FULL_MAIN_COMMIT_SHA
```

The selected commit must belong to the main history available at workflow dispatch
and have successful native main-push `ci complete` evidence. Unmerged PRs and fork
commits cannot publish. Intermediate commits without a main-push CI run are not
admitted. A later main merge does not cancel or change the selected checkpoint.

Release builds the complete distribution once and runs generated-source,
PostgreSQL and browser integration checks in parallel. Both the exact artifact
build/consumer and integration checks must succeed before publication. Full
builder fixtures, infrastructure checks, race tests and boot reproducibility can
be run explicitly when needed; they are not universal release gates.

The credential-free build produces CLI, SDK/proto, builder, Control Plane, platform,
host and guest artifacts. Trusted publishers execute only the admitted workflow
revision, parse candidate metadata as data, and never execute candidate code with
publication credentials. Public consumer execution happens in a separate job with
no publishing identity. Release publishes OCI/npm assets and completes the signed
`release-index.json` only after the downloaded consumer succeeds.

This workflow does not update a managed environment or build managed AMIs. A
consumer selects the completed version and exact signed index digest, reuses those
bytes, and owns its deployment. A failed build/integration run cannot publish or
trigger an environment update. Publishing assets is not completion: consumers must
require the signed complete index.

## Identity and ordering

Preview versions are `v<core>-preview.g<short-sha>.b<release-run-id>`.
The release run owns `build.runId`; `ciRun` identifies prior exact-source CI.
`sourceCommit` is the selected main commit and `workflowCommit` is the trusted
release workflow revision. They need not match. SDK/proto and CLI/Worker metadata
bind the selected source and build; existing signed v0 index fields remain stable.

One native workflow concurrency group serializes complete checkpoint releases,
without cancelling active runs or evicting queued requests. Before any npm or OCI
write, preview publication rejects a source older than, or unrelated to, the last
published checkpoint. A fresh run for the same source may replace the shared
preview channel only when its build run ID is newer. Discovery uses the same
ordering with conditional pointer writes; retrying an older completed run cannot
move the pointer backwards. Main moving ahead alone is not supersession.

Preview assets are stored at the fixed origin
`https://helmr-previews-879980497511-us-east-1.s3.us-east-1.amazonaws.com`
under `previews/{version}/`. `channels/preview.json` identifies the completed main
checkpoint. Conditional create-only asset writes verify existing bytes on retries;
the mutable channel pointer uses compare-and-swap. The signed completion index is
written last. OCI/npm assets must match their original exact bytes/digests.

## Retry and stable tags

Successful build parts are frozen as native Actions artifacts named
`build-artifacts-<release-run-id>-<part>`, retained for seven days. A retry restores
present original parts, and missing never-published parts can build. Ambiguous or
expired listed artifacts fail. Published bytes are never silently replaced.
Start a fresh dispatch for a new source or a new build identity.

Stable `v*` tags use the same release-owned build and integration path, require
successful main source CI, and publish through GitHub Releases. Generated preview
tags are excluded. Human-tag runs remain single-attempt; a partially published tag
requires a new tag rather than a retry with replacement bytes.

Tag publishers export immutable signed readback artifacts identified by publisher
run, attempt, artifact ID and digest. The unprivileged verifier checks native ZIP
identity, exact file coverage, signatures and all asset hashes before running the
consumer. The privileged finalizer rechecks the verified bytes and writes the
completion index last. Missing or invalid readback never triggers a rebuild.

For local release policy validation:

```sh
nix run .#ci-policy
```

Local checks do not establish hosted dispatch identity, npm/Sigstore publication,
or environment readiness. Actual publication and deployment are separate operations.

## Pinned release tools

Artifact construction retains `nix develop .#images`. Fresh trusted admission,
publication, completion and discovery jobs use `nix develop .#release`, a
compiler-free shell shared across the release lifecycle.

The release shell contains only Bash/coreutils, Python, curl, Git, AWS CLI,
cosign, skopeo and the Product-pinned Node/npm. Python handles native GitHub APIs,
archive transport and byte verification; Git handles source admission and discovery
ancestry. AWS CLI and curl handle preview role assumption and conditional object
writes. cosign is required in admission as well as publication/completion, since
already-complete cohorts must pass signature verification. skopeo preserves and
checks OCI digests; npm publishes the exact package tarballs with provenance.
The existing Node archive pin supplies npm, replacing the separate setup-node
runtime. No executable cache is added to these trusted jobs.

The fresh unprivileged verifier uses `nix develop .#release-consumer`, sharing
transport/signature/Node tools but adding the Docker client with Buildx and the installer's
tar/gzip/awk/sed commands. It has no AWS CLI requirement. The downloaded CLI uses
host Node for configuration and Docker for compilation inside the canonical
builder, so this shell needs no Go, C compiler, Bun or SquashFS encoder. Formal
tag signature verification, public OCI checks, real downloaded CLI/installed npm
consumption, completion signing and final public readback retain their existing
job and credential boundaries.

`nix flake check` executes every required tool without a runner/stdenv PATH and
checks that source compilers are absent from both toolsets. These checks do not
replace a native Linux consumer run or normal main-preview publication. Compare
the realized closure and setup duration with `images` on the same runner/cache
condition; a smaller declared toolset alone is not a measured download/time saving.

## Concrete installer and self-host consumers

The dependency-light `install` script (also served by `helmr.dev/install`) downloads
`helmr-<os>-<arch>.tar.gz`, `checksums.txt`, and `release-index.json` from the selected
release. Stable discovery requires the common CLI asset, checksum projection and
completed index/signature inventory through GitHub releases. Explicit preview versions
(`-preview.` in the tag) fetch from the fixed preview origin only, with no redirects;
the shell bootstrap verifies the completion index before any binary download. Its
bootstrap trust is HTTPS plus index/checksum binding; it does not perform Sigstore
verification or install a new mandatory runtime.

Self-host Runtime publication uses `publish-platform-release.sh STORE TAG INDEX
INDEX_SIGNATURE ARCHIVE PROVENANCE`. It verifies the signed v0 index with the exact
tag/main workflow identity, matches the checkout/tag/source and indexed archive and
provenance, safely extracts, and delegates to the existing immutable publisher.
No standalone Platform archive signature is produced or accepted.
