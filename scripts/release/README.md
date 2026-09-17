# Common Product releases (v0)

`release.yaml` owns stable tags and previews. It builds and verifies Product
artifacts, publishes OCI/npm assets, then completes one signed `release-index.json`.
It does not prepare managed AMIs or update staging. Cloud selects immutable bytes
and owns capacity, ECR/S3 preparation, AMIs and deployment.

Both `release.yaml` and `ci.yaml` use `build-artifacts.yaml` (displayed as
**build release artifacts**); CI first runs `artifact-selection` (**select artifact
source**). Frozen GitHub build artifacts are named
`build-artifacts-<run-id>-<part>` for upload and retry restoration.

## Source selection and bootstrap

A successful **main push** through native `ci.yaml` and its final `ci complete`
aggregate starts an automatic preview. Admission verifies native repository,
workflow ID/path, event, SHA and job conclusion. Generated `v*-preview.*` tags
are explicitly excluded from ordinary tag triggers. Root `README.md` and
`packages/web/src/content/docs/**` are the only documentation-only paths; SDK/proto
READMEs, LICENSE, locks, workflows and unknown paths remain relevant. Comparisons
include deletions/renames and the entire diff since the last selected complete
source, so a relevant change followed by docs cannot disappear.

After the reviewed mechanism is on main, an operator can select an **unmerged** PR:

```
gh workflow run release.yaml --ref main -f pr=PR_NUMBER -f commit=FULL_CURRENT_HEAD_SHA
```

This command is an operational action requiring separate authorization. Admission
requires an open same-repository PR to main whose current head equals the full SHA,
successful native PR CI associated with that head, and an identical `.github/workflows`
Git tree to current Product main. The tree comparison includes names, content and
modes; it also rejects a behind PR with older workflows even when its changed-files
list contains no workflows. This restriction applies only to manual PR publication:
`GITHUB_TOKEN` lacks the workflow-write authority needed for native release create
**and update** when workflow contents differ. Precreating a tag alone is not an
established solution. Match main workflows, then select the new full PR head SHA.
Normal PR CI keeps its
merge-result checkout. Native CI run `head_sha` identifies the PR head; it does not
mean every merge-result test used that checkout. `build-artifacts` additionally
builds the exact head. The main-owned manual graph repeats exact-source builds,
selected Go checks, generated-runtime checks and the real consumer fixture under
trusted orchestration. Candidate source has no publication credentials. Privileged
jobs check out only the admitted workflow commit and parse candidate outputs as
data; they never execute candidate scripts or binaries. The current PR head is
rechecked together with current main workflow equality before staging external
writes and again before final publication. A main move that changes workflows
therefore stops publication at the next gate. GitHub remains authoritative if main
moves after a check; these read checks are not an atomic lock on main. Automatic
main previews and formal tag admission keep their existing semantics.

The mechanism PR's uncredentialed artifact build is the first integration gate.
It needs neither published builder digests nor the separate infrastructure behavior
prerequisite on main. The fixture derives the builder digest from actual OCI
manifest bytes, transfers that image to a loopback registry, and uses the same CLI
constructor with that local digest reference. Subsequent Product infrastructure
prerequisites with workflows matching current main can be published and
staging-verified from an unmerged PR.

## Identity, publication and retry

Preview versions are `v<core>-preview.g<short-sha>.b<GitHub-run-id>`; the `g` avoids
invalid numeric prerelease identifiers such as `01234567`. The fixed v0 index binds
the full source SHA/ref, separate workflow SHA/ref, original build run/attempt,
CI run, and fixed assets. SDK and proto carry the same exact source/build stamp;
SDK depends on precisely its sibling proto version. CLI binaries embed the canonical
builder digest. Existing platform and Worker descriptors remain authoritative.
The Linux host constructor asserts the actual Worker's full source/version output,
including path-flake builds where `self.rev` is unavailable.

Every successful build part is frozen in a native Actions artifact named by build
run and component. Preview retries may reuse those original bytes across publishing
attempts. Missing original parts may build; expired/ambiguous original transfers
fail and require a new run identity. Existing npm, OCI and GitHub assets must match
exact bytes/digests. Human tag cohorts retain single-attempt protection, including
failed-job reruns; use a new human tag after any partial publication.

The draft release contains the fixed assets and a signed pre-completion copy of
the index (`release-build.json`). The privileged publisher locates the unique
version through paginated native release listings, then downloads and verifies
that draft's signed build and all fixed assets. GitHub's tag endpoint exposes
published releases; it is not a draft lookup. Duplicate versions fail rather than
selecting one. These checks do not lock out another authorized writer.

The publisher uploads this fresh, flat readback directory as the immutable native
Actions artifact `release-readback-<run-id>-<publisher-attempt>` (compression 0,
7-day retention, no overwrite). Its outputs carry the release ID, raw signed-build
digest, publisher attempt, artifact ID and artifact digest. The action digest is
bare lowercase SHA-256 hex; the downloader validates it and adds `sha256:` once
for comparison with native REST metadata and downloaded ZIP bytes. The build
digest already uses Product's `sha256:<hex>` shape.

The verifier has only Contents/Actions read permissions and no release environment
or OIDC grant. It downloads the exact artifact ID, checks run/name/expiry/digests,
requires precisely the fixed assets and build/signature as regular flat files,
then verifies the signature, selection and every asset before execution. Its
candidate execution step receives no GitHub token. There is no direct draft-read
fallback and no extra release manifest. Missing outputs, expired handoffs or
invalid bytes stop verification without invoking a rebuild. This does not change
the separate missing-build-part behavior described above.

After consumer success, the privileged finalizer re-reads the same unique release
ID and checks the expected raw signed-build digest and all assets. It uploads
`release-index.sigstore.json`, then `release-index.json` **last**, with index bytes
identical to the verified build, and publishes the draft. Anonymous completed-byte
readback must then pass. Retries reuse frozen original bytes/signatures and never
overwrite assets. An incomplete draft or missing completion index is unusable.

A verifier-only retry uses retained publisher outputs: build attempt 1, publisher
attempt 2 and verifier attempt 3 can legitimately differ. Re-running publish uses
the executing publisher attempt for a fresh handoff, while retaining the original
signed build attempt. A completion-only retry needs the successful verify and
publisher outputs, not an unexpired transfer. If publishing the draft succeeded
but its response or public readback failed, completion first binds the release ID
and build digest, requires signed completed-index bytes to equal those build bytes,
and repeats public readback with **zero signing, uploads or PATCHes**. Full preview
rerun admission can use public completed bytes without saved publisher outputs;
incomplete cohorts remain subject to retained build-part availability. Human tag
single-attempt rules are unchanged. Hosted partial-rerun output retention and the
pinned upload's actual ZIP transfer still require operational proof.

Preview signature identity is exactly:
`https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/heads/main`.
Human tags use `@refs/tags/<version>`. OCI copying uses `--insecure-policy`
explicitly because image trust comes from this signed index and preserved manifest
digests, not a separate containers/image signature policy installed on the host. Workflow commit is signed metadata/native
provenance, not an extra suffix in the certificate identity. Cloud pins the SHA-256
of the exact signed index bytes and verifies the native Sigstore issuer/identity.

The reserved GitHub `preview` release's notes are the only discovery pointer:
version, source SHA and exact signed index digest. The short updater is serialized.
Pinned Actionlint 1.7.9 rejects GitHub's `queue: max`, so every updater reconciles
**all completed signed main previews** from the existing release surface. This
handles a late stale completion replacing a newer pending updater. Ancestry and
relevance guards prevent regression; discovery failure can retry against already
complete bytes without rebuilding/publishing. There is no second registry of state.

`npm --tag unlisted` is upload machinery for preview/prerelease versions, not a
supported consumption channel. Consumers install the exact index-selected version.
Published assets and their OCI/npm closure have no automatic deletion initially;
a public support window is a later decision. Native Actions transfer retention is
7 days and is not the release retention policy. Nix content reuse and Cloud's
actual AMI fingerprint provide reuse; stamped binaries/packages may rebuild.

## Validation and activation boundaries

`nix develop .#images -c tests/release_workflow_test.sh` runs admission, frozen-byte,
index-last, npm collision and discovery failure fixtures. The artifact build workflow
builds actual SDK/proto, CLI targets, builder, CP/publisher, platform, host and guest
assets. `tests/release_consumer.sh ASSETS` installs real packages through a loopback
npm fixture, typechecks/executes SDK calls, downloads the CLI and builds a real
bundle using the canonical builder. No synthetic SDK or version-only test stands
in for consumption. The post-publication consumer also requires anonymous OCI
access. Local fixtures do not establish native Sigstore issuance or public access.

Before activation, separately verify npm trusted-publisher authorization for
main-executed `release.yaml` and Environment `release`, GitHub Environment branch
rules (exact `main` branch plus existing `v*` tag pattern, retaining the required
reviewer), GHCR visibility, native immutable releases, and
retention settings. PR workflows retain merge checks plus an additional full artifact
build: runner time/disk demand increases. The graph records disk/memory/output
sizes on `ubuntu-24.04`; native Linux hosted feasibility is an acceptance requirement,
not an assumption. Cloud previously required a larger runner after ENOSPC; this
source does not grant public access to that runner or change paid-runner settings.

Live publication, downloaded-binary S3 execution, managed AMIs and staging runtime
integration remain separate operational proof. Preserve the real deployed pin and
retained-generation/state/IAM guards until the separately reviewed prerequisite
and actual published selection are available. A rollback of artifacts alone does
not establish database rollback safety.

## Concrete installer and self-host consumers

The dependency-light `install` script (also served by `helmr.dev/install`) downloads
`helmr-<os>-<arch>.tar.gz`, `checksums.txt`, and `release-index.json` from the selected
release. Stable discovery requires the common CLI asset, checksum projection and
completed index/signature inventory. Explicit preview tags use the same names.
The CLI constructor derives the four-line SHA-256 checksum file; the signed index
binds that file, and publisher validation requires it to equal the CLI asset hashes.
It is not another release state or signing protocol. The shell bootstrap verifies
the checksum file against the canonical index and then the archive checksum, with
only the existing shell/curl/tar/hash tools. Its bootstrap trust is HTTPS; it does
not claim local Sigstore verification or install a new mandatory runtime. Privileged
consumers still verify Sigstore explicitly. The release consumer fixture executes this
shipped installer using a loopback release transport before running the actual CLI.

Self-host Runtime publication uses `publish-platform-release.sh STORE TAG INDEX
INDEX_SIGNATURE ARCHIVE PROVENANCE`. It verifies the signed v0 index with the exact
tag/main workflow identity, matches the checkout/tag/source and indexed archive and
provenance, safely extracts, and delegates to the existing immutable publisher.
No standalone Platform archive signature is produced or accepted.
