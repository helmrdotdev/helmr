# Common Product releases (v0)

`release.yaml` owns stable tags and previews. It builds and verifies Product
artifacts, publishes OCI/npm assets, then completes one signed `release-index.json`.
It does not prepare managed AMIs or update staging. Cloud selects immutable bytes
and owns capacity, ECR/S3 preparation, AMIs and deployment.

Both `release.yaml` and `ci.yaml` use `build-artifacts.yaml` (displayed as
**build release artifacts**); CI first runs `artifact-selection` (**select artifact
source**). On **main push**, CI builds once; successful automatic preview
publication adopts that exact producer selection and frozen bytes without
recompiling. The release publisher keeps its own run for readback artifacts
(`release-readback-<publisher-run>-<publisher-attempt>`) while restore/assemble
use the CI producer run. The signed index binds the original CI build run and
fixed assets.
Frozen GitHub build artifacts are named
`build-artifacts-<run-id>-<part>` for upload and retry restoration.

## Source selection and bootstrap

A successful **main push** through native `ci.yaml` starts an automatic preview
after both aggregates succeed:

- **source-ci-complete** — required source checks (Nix, repo, postgres, browser,
  bundle-builder, release contracts). This aggregate does not bless skipped or
  failed artifact jobs.
- **preview-ready** — on main only, requires successful artifact build and
  consumer verification when changes are relevant; documentation-only changes
  that provably cannot reach artifacts skip the expensive build with a truthful
  successful skip job instead.

Admission verifies native repository, workflow ID/path, event, SHA, both main
aggregates, and preview-ready artifact/consumer success for the exact producer
commit when publication is relevant. Documentation-only main pushes skip publication
when admission observes the producer CI run's successful **artifact build skipped**
job; release admission does not re-derive that decision from the preview pointer.
Generated `v*-preview.*` tags are
explicitly excluded from ordinary tag triggers. Root `README.md` and
`packages/web/src/content/docs/**` are the only documentation-only paths; SDK/proto
READMEs, LICENSE, locks, workflows and unknown paths remain relevant. Comparisons
include deletions/renames and the entire diff since the last selected complete
source, so a relevant change followed by docs cannot disappear.

Pull requests retain **ci complete**, which requires **source-ci-complete** and
all packaging work selected for the exact PR head. Ordinary application/Console
changes retain source builds and tests without creating the full distribution.
Release publication for PRs and human tags still builds the complete set once
under trusted orchestration; a lightweight PR result never substitutes for it.

### PR work selection

`scripts/release/ci_policy.py` classifies the full PR merge-base-to-head diff.
Source checks use GitHub's native merge result; selected distribution artifacts
use the exact head. Renames include both names. Missing ancestry, empty diffs,
unknown paths, shared dependencies, locks, build/release/CI wiring and
CLI/SDK/proto/compiler/runtime/Worker changes select full work. Main keeps complete
source validation and builds every artifact-relevant change, including ordinary
application edits. Its documentation-only skip policy above is unchanged.

| PR path group | Distribution set / builder E2E | Retained proof |
| --- | --- | --- |
| Root README and `packages/web/src/content/docs/**` | Skip / skip | All ordinary source checks, including website build |
| `packages/console/src/**` (`.ts`, `.tsx`, `.css`) | Skip / skip | TypeScript tests, generated output, embedded-Console Go build/race/lint and browser tests |
| `packages/web/src/**` (`.ts`, `.astro`, `.css`) and public SVG/PNG/ICO/webmanifest assets | Skip / skip | Website typecheck and real website build, plus all other source checks |
| Control Plane `project.go`, `organization.go`, `member.go` and `project_*`, `organization_*`, `member_*` Go test files; `internal/email/**.go` | Skip / skip | Go build/race/lint, PostgreSQL, browser and source contracts |
| Existing Console embedding Go files, Console Vite config/index, Control Plane image build/verify scripts | Full / skip | Complete package consumer plus source checks; these do not reach builder fixtures |
| Every other or mixed path class | Full / full when any member requires it | Complete distribution/consumer, deep builder tests and source checks |

A maintainer can add **`ci:full`** to request both expensive checks on any PR.
Adding/removing labels and pushing a new revision recompute selection and all
required checks; unrelated label events also run normal CI, rather than leaving a
skipped or misleading required status. The label only requests computation and
works on fork PRs without granting release credentials. Obsolete PR runs cancel;
main runs do not.

Both aggregates reject failed/cancelled/missing selected jobs, failed selection,
invalid boolean outputs and unexpectedly skipped mandatory jobs. Only an explicit
selection permits `skipped` for optional work. A distribution-only defect in an
ordinary source edit can first be discovered on main, where complete build and
consumer success are still required before publication. Use `ci:full` for earlier
package-level proof when a change warrants it.

Ordinary PRs upload no distribution set. When a selected full PR build needs
frozen artifacts for its consumer/retry, those transfers last **one day**. Main,
stable tags and manual PR preview builds retain the existing **seven-day** handoff
window. Browser failure evidence keeps its separate retention. No new test
artifact upload is introduced. On PR retries, present frozen parts can be restored,
but **any missing part requires a new workflow run**, including a part that never
finished building. Expired/deleted artifacts cannot reliably be distinguished from
never-built parts by their absence. A new commit or label change starts a new run;
GitHub's rerun button keeps the old run ID. Main/tag/manual preview retry behavior
is unchanged. No missing PR part is silently recreated under its old identity.

After the reviewed mechanism is on main, an operator can select an **unmerged** PR:

```
gh workflow run release.yaml --ref main -f pr=PR_NUMBER -f commit=FULL_CURRENT_HEAD_SHA
```

This command is an operational action requiring separate authorization. Admission
requires an open same-repository PR to main whose current head equals the full SHA,
successful native PR CI associated with that head, and exact native source validation.
Normal PR CI keeps its
merge-result checkout. Native CI run `head_sha` identifies the PR head; it does not
mean every merge-result test used that checkout. `build-artifacts` additionally
builds the exact head. The main-owned manual graph repeats exact-source builds,
selected Go checks, generated-runtime checks and the real consumer fixture under
trusted orchestration. Candidate source has no publication credentials. Privileged
jobs check out only the admitted workflow commit and parse candidate outputs as
data; they never execute candidate scripts or binaries. The current PR head is
rechecked before staging external writes and again before final publication.
GitHub remains authoritative if main moves after a check; these read checks are
not an atomic lock on main. Automatic main previews and formal tag admission keep
their existing semantics.

The mechanism PR's uncredentialed artifact build is the first integration gate.
It needs neither published builder digests nor the separate infrastructure behavior
prerequisite on main. The fixture derives the builder digest from actual OCI
manifest bytes, transfers that image to a loopback registry, and uses the same CLI
constructor with that local digest reference. Subsequent Product infrastructure
prerequisites with workflows matching current main can be published and
staging-verified from an unmerged PR.

## Identity, publication and retry

Preview versions are `v<core>-preview.g<short-sha>.b<GitHub-run-id>`; the `g` avoids
invalid numeric prerelease identifiers such as `01234567`. Main previews key the
version to the **CI producer run id**. The fixed v0 index binds the full source
SHA/ref, producer workflow commit (the admitted push SHA), signer workflow ref,
original build run, CI run, and fixed assets. SDK and proto carry the same exact source/build stamp;
SDK depends on precisely its sibling proto version. CLI binaries embed the canonical
builder digest. Existing platform and Worker descriptors remain authoritative.
The Linux host constructor asserts the actual Worker's full source/version output,
including path-flake builds where `self.rev` is unavailable.

Every successful build part is frozen in a native Actions artifact named by the
**producer CI build** run and component. Main preview publication restores those
CI bytes directly; release retries may reuse them without rebuilding. Automatic main previews serialize release runs per CI producer
(`cancel-in-progress: false`) so competing runs for the same producer cannot cancel in-flight publication.
Automatic main previews and PR previews publish to the fixed-origin object store
(`PREVIEW_BASE_URL`, default
`https://helmr-previews-879980497511-us-east-1.s3.us-east-1.amazonaws.com`) under
`previews/{version}/`, with `channels/preview.json` as the main discovery pointer.
Writes use conditional `put-object` (`If-None-Match: *`); existing objects are
verified byte-for-byte, never overwritten or deleted. Anonymous reads may return
403 instead of 404 for missing keys; optional probes treat `(403)` and `(AccessDenied)` as unavailable,
never as success. Publisher reads that require an existing object treat those codes as hard errors.
A `(ConditionalRequestConflict)` / `(409)` on conditional put fails closed; re-run the whole release workflow.
Stage uploads public assets only; `release-build.json` remains
a local verifier input and is never written to the object store. The unprivileged
verify job reconstructs the unsigned build index from public blobs; completion
signs `release-index.json` last. Formal human tags retain the GitHub release
draft/readback path.

Producer, main preview-channel and discovery concurrency use GitHub's native `queue: max`
(up to 100 pending per group; no single pending eviction). After obtaining the
publish job's preview-channel lock, automatic main publication re-checks Product
`main` HEAD and publishes **only when the admitted source is still current head**.
Superseded candidates exit successfully before any artifact, npm or OCI write, and
verify/complete stay skipped; re-running a superseded release run skips again.
A publisher already holding the lock continues with its admitted bytes even if
main moves before it finishes. While current `main` is red or its preview has not
yet completed, older green commits do not publish and the last completed preview
remains available. This head coalescing avoids moving npm `preview` backwards and
does not silently substitute latest bytes for an obsolete CI run. Missing original
parts may build on PR/tag paths; expired or
ambiguous original transfers fail and require a new run identity. Existing npm,
OCI and GitHub assets must match exact bytes/digests. Human tag cohorts retain
single-attempt protection, including failed-job reruns; use a new human tag after
any partial publication.

The draft release contains the fixed assets and a signed pre-completion copy of
the index (`release-build.json`). The privileged publisher locates the unique
version through paginated native release listings, then downloads and verifies
that draft's signed build and all fixed assets. GitHub's tag endpoint exposes
published releases; it is not a draft lookup. Duplicate versions fail rather than
selecting one. These checks do not lock out another authorized writer.

The publisher uploads this fresh, flat readback directory as the immutable native
Actions artifact `release-readback-<publisher-run>-<publisher-attempt>` (compression 0,
7-day retention, no overwrite). Its outputs carry the release ID, publisher run,
raw signed-build digest, publisher attempt, artifact ID and artifact digest. The action digest is
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

A verifier-only retry uses retained publisher outputs: publisher
attempt 2 can differ from verifier attempt 3. Re-running publish uses
the executing publisher attempt for a fresh handoff while retaining frozen
build bytes. A completion-only retry needs the successful verify and
publisher outputs, not an unexpired transfer. If publishing the draft succeeded
but its response or public readback failed, completion first binds the release ID
and build digest, requires signed completed-index bytes to equal those build bytes,
and repeats public readback with **zero signing, uploads or PATCHes**. Full preview
rerun admission can use public completed bytes without saved publisher outputs;
incomplete cohorts remain subject to retained build-part availability. Human tag
single-attempt rules are unchanged. Hosted partial-rerun output retention and the
pinned upload's actual ZIP transfer still require operational proof.

Product publishes exactly `ghcr.io/helmrdotdev/bundle-builder` and
`ghcr.io/helmrdotdev/control-plane`, each selected by immutable manifest digest.
The fixed OCI names in `contract.py` are separate from GitHub source/workflow/API
repository identity (`helmrdotdev/helmr`). Producers read the same fixed map that
validates descriptors and the CLI's builder input; nested or foreign names are
rejected. Both actual image constructors set `org.opencontainers.image.source`
to `https://github.com/helmrdotdev/helmr`. Labels change image bytes, so renamed
images require a new cohort; existing frozen releases cannot be rewritten.
Repository linkage and public package visibility are separate native GitHub
settings. Anonymous OCI verification must succeed before release completion.
Worker remains host/runtime bundles and AMIs; npm and Cloud ECR names are unchanged.

Preview signature identity is exactly:
`https://github.com/helmrdotdev/helmr/.github/workflows/release.yaml@refs/heads/main`.
Human tags use `@refs/tags/<version>`. OCI copying uses `--insecure-policy`
explicitly because image trust comes from this signed index and preserved manifest
digests, not a separate containers/image signature policy installed on the host. Workflow commit is signed metadata/native
provenance, not an extra suffix in the certificate identity. Cloud pins the SHA-256
of the exact signed index bytes and verifies the native Sigstore issuer/identity.

The main preview pointer at `channels/preview.json` binds one completed cohort:
version, source SHA and signed index digest. Discovery updates it with a conditional
CAS from the snapshot validated in that decision; ancestry and relevance guards prevent
regression. There is no GitHub release scan or second registry of preview history.

`npm publish --tag` channels are explicit install pointers: main previews use
`preview`, manual PR publication uses `pr-<number>` from selection identity (not
inferred from the version string), formal prereleases use `next`, and stable human
tags use `latest`. Tags move during staging one package at a time and are not
atomic cohort readiness signals. The signed completed `release-index.json` and
discovery pointer remain authoritative for which bytes are released.
Same-byte npm retries compare tarball bytes and never replace an existing version.
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
