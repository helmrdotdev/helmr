#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

require_text() {
  grep -Fq -- "$1" "$2" || fail "$3"
}

reject_text() {
  if grep -Fq -- "$1" "$2"; then
    fail "$3"
  fi
}

for command in helmr-controlplane helmr-dispatcher helmr-worker; do
  [ ! -e "${root}/cmd/${command}" ] || fail "removed cmd/${command} still exists"
done

for file in Makefile scripts/build-controlplane-image.sh scripts/ci-postgres.sh \
  scripts/publish-materialized-platform-release.sh nix/packages/worker.nix; do
  reject_text 'cmd/helmr-' "${root}/${file}" "${file} still builds a removed command path"
done
reject_text 'bin/helmr-worker' "${root}/nix/packages/worker.nix" \
  "Nix still emits helmr-worker"
reject_text 'bin/helmr-worker' "${root}/nix/packages/default.nix" \
  "workerHost still emits helmr-worker"

require_text 'ENTRYPOINT ["/usr/local/bin/control-plane"]' \
  "${root}/scripts/build-controlplane-image.sh" "Control Plane image entrypoint is not canonical"
reject_text 'ENTRYPOINT ["/usr/local/bin/helmr-controlplane"]' \
  "${root}/scripts/build-controlplane-image.sh" "removed Control Plane image entrypoint remains"
reject_text 'COPY helmr-controlplane /usr/local/bin/helmr-controlplane' \
  "${root}/scripts/build-controlplane-image.sh" "removed Control Plane executable output remains"
reject_text 'COPY helmr-dispatcher /usr/local/bin/helmr-dispatcher' \
  "${root}/scripts/build-controlplane-image.sh" "removed Dispatcher executable output remains"
require_text 'default     = ["/usr/local/bin/control-plane"]' \
  "${root}/infra/aws/modules/controlplane/variables.tf" "Terraform Control Plane entrypoint is not canonical"
require_text 'entryPoint = ["dispatcher"]' \
  "${root}/infra/aws/modules/controlplane/main.tf" "Terraform dispatcher entrypoint is not canonical"

require_text 'files=(cpu-template-helper firecracker jailer mkfs.ext4 worker)' \
  "${root}/scripts/materialize-worker-host-bundle.sh" "Worker bundle member list is not canonical"
reject_text 'files=(cpu-template-helper firecracker helmr-worker' \
  "${root}/scripts/materialize-worker-host-bundle.sh" "removed Worker bundle member remains"
reject_text 'helmr-worker' "${root}/scripts/materialize-worker-host-bundle.sh" \
  "Worker bundle producer still names the removed member"
reject_text '"helmr-worker"' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker bundle manifest or installer still names the removed member"
reject_text 'firecracker helmr-worker' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker bundle installer still names the removed member"
reject_text '/usr/local/bin/helmr-worker' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker AMI still installs or starts the removed executable"
require_text 'ExecStart=/usr/local/bin/worker' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker AMI unit does not start the canonical binary"

require_text 'CONTROLPLANE_IMAGE_REPOSITORY: ghcr.io/${{ github.repository }}/control-plane' \
  "${root}/.github/workflows/release.yaml" "release workflow does not publish the nested Control Plane package"
reject_text 'ghcr.io/${{ github.repository_owner }}/helmr-controlplane' \
  "${root}/.github/workflows/release.yaml" "removed Control Plane package publication path remains"

require_text 'WorkerTokenIssuer         = "helmr-controlplane"' \
  "${root}/internal/auth/worker.go" "Worker JWT issuer changed"
require_text 'WorkerTokenAudience       = "helmr-worker"' \
  "${root}/internal/auth/worker.go" "Worker JWT audience changed"
require_text 'otelhttp.NewMiddleware("helmr-controlplane")' \
  "${root}/internal/controlplane/server.go" "Control Plane telemetry label changed"
require_text 'request.Header.Set("user-agent", "helmr-controlplane")' \
  "${root}/internal/controlplane/oauth.go" "Control Plane OAuth user-agent changed"
require_text 'default     = "helmr-worker"' \
  "${root}/infra/aws/modules/worker/variables.tf" "Worker systemd service identity changed"
require_text 'cat >/etc/systemd/system/helmr-worker.service' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker systemd unit path changed"
require_text '/system.slice/helmr-worker.service/supervisor' \
  "${root}/internal/worker/verifier_host_linux_test.go" "Worker unit-derived cgroup identity changed"
require_text 'controlplaneImage' "${root}/scripts/write-aws-release-manifest.sh" \
  "release manifest field identity changed"
require_text 'helmr-worker-enrollment-token' \
  "${root}/infra/aws/modules/worker/templates/user-data.sh.tftpl" \
  "Worker enrollment-token identity changed"
require_text '/var/log/helmr-worker-drain.log' \
  "${root}/infra/aws/modules/worker/templates/user-data.sh.tftpl" "Worker drain-log path changed"
require_text 'filepath.Join(os.TempDir(), "helmr-worker")' \
  "${root}/internal/executor/executor.go" "Worker temporary state path changed"
require_text 'filepath.Join(os.TempDir(), "helmr-worker", "vms", "guest")' \
  "${root}/internal/firecracker/config.go" "Firecracker temporary state path changed"
require_text 'WORKER_IMAGE_NAME="${WORKER_IMAGE_NAME:-helmr-worker-image}"' \
  "${root}/scripts/aws-release-artifacts.sh" "Worker image infrastructure identity changed"
require_text '.schema == "helmr.worker-host-artifacts.v0"' \
  "${root}/infra/aws/modules/worker-image/templates/build-worker-image.sh.tftpl" \
  "Worker host-artifact schema identity changed"
require_text 'scripts/build-controlplane-image.sh' "${root}/tests/release_workflow_test.sh" \
  "internal Control Plane image-builder filename changed"

printf 'ok - runtime naming contract\n'
