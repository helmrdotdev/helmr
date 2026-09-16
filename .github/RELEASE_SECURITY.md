# Release security

The reviewed `release.yaml` workflow publishes one common Product artifact set:
Control Plane and bundle-builder OCI images, CLI binaries, SDK/proto packages,
the Platform Runtime closure, and Worker host/runtime bundles. Managed AMIs,
AWS preparation and deployment belong to Cloud. Publication does not update staging.

Successful native main-push CI admits automatic previews. Manual previews select
an open same-repository PR to main by its current full head SHA, after successful
native PR CI. Its `.github/workflows` Git tree must equal current main, including
for behind PRs; native release create/update with differing workflows requires
workflow-write authority unavailable to `GITHUB_TOKEN`. Admission rejects this
before building, and publication rechecks the current PR head and main workflow
tree before staging writes and finalization. No new credential or tag workaround
is used. Normal merge-result checks remain; the trusted main workflow rebuilds
and checks the exact selected source without publication credentials. Privileged
jobs execute only reviewed workflow source and treat candidate output as data.
The manual publication path is available only after this mechanism is on main.
Human tags use the same constructors and retain single-attempt publication safety.

The release Environment gates signing and publication. The fixed v0 index binds
source SHA/ref, workflow SHA/ref, build identity and every common asset digest.
Preview Sigstore identity is the exact main `release.yaml` identity; human tags use
the corresponding exact tag identity. Workflow commit is signed metadata, not a
certificate suffix. A signed draft permits a fresh unprivileged consumer to install
the actual SDK/proto and build a bundle with the downloaded CLI and canonical
builder. Only then is the signed completion index published last. Public readback
checks the exact signed bytes and asset hashes.

Preview retries reuse frozen original bytes and reject same-version collisions.
A failed human tag cohort requires a new tag. Completed signed main previews are
reconciled by the serialized discovery updater with ancestry/relevance guards.
The reserved GitHub preview release is the only discovery pointer. Artifacts have
no automatic deletion initially; retirement and any public support window are
separate decisions.

The builder owns dependency installation and compilation. The CLI embeds its
registry digest and uploads only verified content-addressed output. Build
credentials remain producer-local. Control Plane verifies closure, formats, sizes,
architecture and Runtime identity; it never rebuilds application artifacts.
The existing immutable Platform-store, retained-object, state and IAM guards
remain required. This source does not authorize operational settings changes.

See [common release operations and bootstrap gaps](../scripts/release/README.md)
for the exact manual interface, runner measurements and npm/GitHub/GHCR settings
that still require separate operational verification.
