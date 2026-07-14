# Helmr AWS Dev Stack

This stack is the deployable AWS development and full-run smoke environment for Helmr.

It creates Secrets Manager containers for Helmr internal and application secrets, but it does not
store those secret values in OpenTofu/Terraform state. The exception is Terraform-managed
ClickHouse Cloud: that path generates the ClickHouse service password, writes it to AWS Secrets
Manager, and therefore stores the generated password in encrypted Terraform state.

Run commands from the repository Nix infra shell so AWS CLI, OpenTofu, and jq versions match the
scripts:

```sh
nix develop .#infra
```

Initialize with an existing S3 backend:

```sh
tofu init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="region=<state-region>"
```

Then apply:

```sh
tofu apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-example" \
  -var="public_url=https://helmr.example.com" \
  -var="control_image=<account>.dkr.ecr.us-east-1.amazonaws.com/helmr-example/control@sha256:<digest>" \
  -var="certificate_arn=arn:aws:acm:..." \
  -var="github_oauth_client_id=Iv1..." \
  -var="create_clickhouse_cloud=true" \
  -var="clickhouse_organization_id=<clickhouse-cloud-organization-id>"
```

When `create_clickhouse_cloud=true`, the stack creates a ClickHouse Cloud service, an AWS
PrivateLink endpoint, private DNS, and the ClickHouse password secret consumed by the control,
dispatcher, and migration tasks. Configure the ClickHouse provider through
`CLICKHOUSE_CLOUD_API_KEY` and `CLICKHOUSE_CLOUD_API_SECRET` in the environment that runs OpenTofu.
For local dev, use `scripts/dev-secrets.sh` so 1Password injects those values only for the command.
Do not put provider API credentials in `.tfvars` or scratch directories.

To bring an existing ClickHouse service instead, keep `create_clickhouse_cloud=false` and pass
`clickhouse_url`, `clickhouse_user`, `clickhouse_password_secret_arn`, and any required
`clickhouse_password_kms_key_arns` or additional client security groups.

This repository does not commit a `.terraform.lock.hcl` because Terraform and OpenTofu
write different provider hostnames. Commit the generated lock file in your deployment repository.

For CloudFront development smoke runs, set `enable_cloudfront=true`, set
`cloudfront_origin_domain_name` to a DNS name covered by `certificate_arn`, and point that origin
name at the ALB. The emitted `control_url` will use the generated `https://*.cloudfront.net`
viewer URL while workers resolve the origin hostname to the internal ALB.

For named dev environments such as `https://dev.helmr.dev`, keep
`create_worker=true` and provide an ACM certificate. The stack derives the worker control URL from
`public_url` and resolves it to an internal ALB inside the VPC; external browser and CLI traffic
continue to use `control_url` and the public DNS record.

The control plane runs as separate `helmr-control` and `helmr-dispatcher` ECS/Fargate services
behind the shared AWS dependencies, including cluster-mode disabled ElastiCache Valkey/Redis for
`HELMR_REDIS_URL`. The stack keeps
`create_control_service=false` by default so first apply can create infrastructure without trying
to pull an image or inject empty secrets. Populate the emitted Secrets Manager secrets, push a
digest-pinned control image to `control_ecr_repository_url`, run the emitted migration task
definition once, then set
`create_control_service=true`. Tune `control_desired_count` and `dispatcher_desired_count`
separately.

The dev stack defaults to low-cost control mode: no NAT Gateway, one control
task, one dispatcher task, short log/object retention, and public IPs for
control/dispatcher/migration Fargate tasks. The task security group still only permits inbound
traffic from the ALB.
Enable NAT Gateway only for run mode or production-like private egress.

Both dev worker groups default to min/warm zero and max one. Worker groups do not exist until
`create_worker=true`; the `dev-worker-tfvars` smoke helper supplies the fleet policy and certified
capacity/cache partitions.
Queued build and run demand then scales the respective group from zero instead of the harness
pinning instances on. Raising either ASG maximum above one additionally requires
`allow_extended_worker_capacity=true`; this makes capacity expansion an explicit cost decision.
CloudWatch alarms project pending readiness, drain age, and unmet capacity but never write desired
capacity. Fleet control rejects auto-detected disk capacity and subtracts the explicit per-role disk
reserve and cache budgets before certifying workload and scratch capacity.
Cost reporting measures the worker instance, root gp3 volume, NAT, control, database, Valkey, load
balancers, private endpoints, and telemetry independently from scaling correctness.

Required secret value formats:

- `database_url`: `postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require`
- `worker_token_signing_key`, `auth_secret`, `setup_token`: high-entropy strings
- `setup_token`: read it from Secrets Manager for first organization setup
- `secret_encryption_key`, `checkpoint_encryption_key`: base64-encoded 32-byte keys
- `github_oauth_client_secret`: GitHub OAuth client secret

The helper script generates the Helmr internal values locally and writes them directly to Secrets
Manager, outside Terraform state:

```sh
../../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `HELMR_DATABASE_URL` and `HELMR_GITHUB_OAUTH_CLIENT_SECRET` to populate external secrets in the same run. Set
`OVERWRITE_SECRETS=1` only when intentionally rotating values.

Email delivery is disabled by default. For Resend, configure:

```hcl
email_provider = "resend"
email_from     = "Helmr <noreply@example.com>"
```

After applying, populate the emitted `secret_arns.resend_api_key` Secrets Manager secret with the
Resend API key before starting the control service.

Run migrations after secrets are populated:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw control_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json control_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json control_task_security_group_ids)" \
    --arg assignPublicIp "$([ "$(tofu output -raw control_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:$assignPublicIp}}')"
```

Worker resources are not created until `create_worker=true`. Build and publish a worker AMI that
satisfies the worker module contract, set `worker_ami_id`, ensure `certificate_arn` and the worker
control DNS name are configured, and apply the fleet policy. Keep minimum and warm capacity at zero
for demand-only smoke runs; queued demand drives capacity through the application controller within
the configured per-role maximum.
Workers are filesystem-first Firecracker hosts; size
`worker_root_volume_size_gb` for build/cache/runtime data and use `worker_disk_mib` only to override
advertised filesystem capacity.

When replacing the worker AMI, keep the previous image in `worker_allowed_ami_ids`, apply the new
control policy and launch template, then explicitly start the Auto Scaling instance refresh. Remove
the old image only after every old instance has terminated. The
`dev-worker-tfvars` helper makes this a two-apply sequence: the first invocation admits the new AMI
without changing the launch template. A successful `dev-apply` records the stable ECS control task
definition and its worker policy. The helper will not change `worker_ami_id` until the OpenTofu
output, that success marker, and the currently stable running control tasks all prove that the
enrollment policy for every worker group contains the new AMI; a failed or partial apply remains in
the staging step.
After the first successful apply, rerun the helper and apply again to start the rolling
replacement. Remove the old AMI explicitly only after the refresh completes.

`dev/aws/db-reset.sh` first requires both application-owned worker fleets to have min and desired
capacity zero with no instances. It then stops control and dispatcher, re-proves both fleets remain
at zero, waits for every captured ECS task ARN to reach `STOPPED`, and only then drops the schema.
It also requires the cluster to contain no remaining running or pending one-off task. If capacity
reappears or physical task stop cannot be proved before the reset,
the prior service desired counts are restored so the application drain can finish. The script never
writes or restores ASG capacity. Once schema mutation starts, services remain stopped on success or
failure so workers cannot boot against an incomplete schema.

For ephemeral full-run smoke testing, use `full-run-smoke.tfvars.example` as the starting point. It
bounds each application-owned fleet at one worker, enables SSM access, and uses EC2 nested
virtualization on `c8i.xlarge`
instead of requiring a bare-metal worker instance. Destroy the stack after the smoke run; it still
creates RDS, Redis/Valkey, NAT, ALB, EC2, and optionally ClickHouse Cloud resources. The repository
scripts expose this as `scripts/aws-dev-smoke.sh dev-destroy` and `scripts/aws-dev-debug.sh
dev-destroy`, which run pre-destroy cleanup before Terraform/OpenTofu destroy. When the stack owns
ClickHouse Cloud, run those commands through `scripts/dev-secrets.sh` so provider credentials are
available only to the destroy process.
When scaling down, finish or cancel workload demand and let the application controller complete its
protected drain to zero while NAT and control remain present. Then use `dev-destroy` to remove the
ephemeral worker topology, NAT, and shared stack resources. The worker topology is not handed back
to Terraform-owned desired capacity in place.
