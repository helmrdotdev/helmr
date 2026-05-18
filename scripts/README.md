# Scripts

Repository maintenance and build helper scripts live here.

Scripts should be small, explicit entrypoints for tasks that do not naturally belong to Go packages, image Makefiles, or CI YAML.

Prefer keeping reusable product logic in Go packages, generated code, or image build definitions; scripts should mainly orchestrate those pieces.

## CI parity

GitHub-hosted CI installs Go, Buf, and Bun, then runs the Go verification path:

```sh
make verify
make test-linux-compile
```

`make test-linux-compile` cross-compiles the Linux-only Firecracker worker
packages without trying to launch a VM. Firecracker execution smoke tests need a
real Linux host with KVM and are intentionally kept out of GitHub-hosted CI.

## AWS dev smoke

`scripts/aws-dev-smoke.sh` orchestrates the AWS dev smoke stack without storing credentials or
secret values. Run it from `nix develop .#infra` after configuring AWS credentials in the shell.

Use `scripts/aws-dev-smoke.sh` for durable environment creation and deploy steps such as worker
AMI builds, control image pushes, migrations, and Terraform/OpenTofu applies.

Use `scripts/aws-dev-debug.sh` for operating an existing dev stack. It discovers the current
control URL, worker Auto Scaling group, and worker instance from Terraform outputs and AWS APIs,
then can fetch worker logs, restart the worker, hotpatch a locally built `guestd`, create a
hello-world run, and print run details/events/logs. Keep generated URLs, instance IDs, and S3
presigned URLs out of source; pass overrides through environment variables when needed.

## Dev environment modes

Local development is the default for product code that does not need external
GitHub callbacks, AWS networking, or a real Firecracker worker. Use the Nix dev
shells for build, unit, CLI, console, and control-plane checks.

AWS control mode is for changes that need managed callbacks, GitHub App OAuth,
CloudFront/ALB behavior, RDS, S3, or ECS. `dev-control-tfvars` enables one
control ECS task by default and scales any existing worker capacity to zero.
It also disables NAT Gateway by default and runs control/migration tasks with
public IPs, while security groups still only allow inbound traffic from the ALB.
Set `DEV_CONTROL_KEEP_WORKER=1` only when intentionally keeping run capacity up.

AWS run mode is for end-to-end run execution through an isolated worker. Use
`dev-worker-tfvars` plus `dev-apply` to make the worker capacity durable in
OpenTofu state; it enables NAT Gateway because private worker hosts need
outbound access to AWS APIs and GitHub. Use `dev-worker-down-tfvars` plus
`dev-apply` to keep worker resources while stopping worker instances after the
run check. If worker capacity was running, apply the down state first so drain
can use NAT, then run `dev-control-tfvars` plus `dev-apply` to remove NAT.

`aws-dev-debug.sh worker-up` and `aws-dev-debug.sh worker-down` are faster
temporary controls for an already-created worker Auto Scaling group. Follow them
with the matching smoke tfvars apply when the desired cost state should be kept
as infrastructure state.

For idle periods, `aws-dev-debug.sh dev-off` scales worker and control capacity
to zero and stops the RDS instance. `aws-dev-debug.sh dev-on` starts the database
and restores the control service. RDS can restart automatically after seven days
when stopped, so destroy long-lived throwaway stacks instead of parking them.

Use `aws-dev-debug.sh worker-image-cleanup` after worker image builds to keep
only the most recent tagged worker AMIs and delete snapshots for older images.

For official worker AMI releases, `aws-dev-smoke.sh worker-image-wait` records both the provider
region AMI ID in `.helmr-aws-dev-smoke/worker-ami-id` and the full region-to-AMI map in
`.helmr-aws-dev-smoke/worker-ami-ids.json`. Use `worker-image-amis` to print the JSON object for
the release workflow's `worker_amis_json` input. Set `STATE_KEY` for release AMI pipelines so they
do not share the dev worker-image stack state.
