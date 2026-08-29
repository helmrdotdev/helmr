# Helmr AWS Quickstart

This example deploys a low-cost Helmr self-hosting baseline for evaluation, PoC, or a startup
environment. It uses the shared AWS modules for network, Control Plane, and optional workers.

Defaults:

- CloudFront is disabled by default. When enabled, it uses the AWS-managed `*.cloudfront.net`
  viewer domain and a separate HTTPS ALB origin DNS name.
- NAT Gateway is disabled.
- Control Plane and migration Fargate tasks run with public IPs, while inbound task traffic still comes
  only from the load balancer security group.
- The `helmr-controlplane` service desired count is `1`.
- The `helmr-dispatcher` service desired count is `1`.
- A single-node, cluster-mode disabled ElastiCache Valkey/Redis instance is provisioned for
  the Control Plane event stream.
- Worker resources are off by default.
- RDS deletion protection is off and final snapshots are skipped so evaluation stacks can be
  destroyed cleanly.
- Short log, secret recovery, KMS deletion, and CAS lifecycle windows keep evaluation costs bounded.

## Deploy

Run from this directory with Terraform or OpenTofu:

```sh
cp terraform.tfvars.example terraform.tfvars
```

Fill `terraform.tfvars` with non-secret values for your AWS region, deployment name,
`helmr_version`, GitHub OAuth client ID, `public_url`, and `certificate_arn`.
Do not put OAuth client secrets, database URLs,
Resend API keys, SMTP passwords, or Helmr signing keys in tfvars.

Initialize and apply:

```sh
tofu init
tofu apply
```

The first apply should usually keep `create_controlplane_service=false`. It creates infrastructure,
resolves the official release artifacts, creates empty Secrets Manager containers, and creates the
migration task definition without trying to start a service that cannot yet read populated secrets.

This example intentionally has no backend block. Add your own backend configuration in the
deployment copy if you need shared remote state.

## Populate Secrets

After the first apply, populate the Secrets Manager ARNs from `tofu output -json secret_arns`.
The stack creates empty secret containers; it does not generate or store Helmr internal secret
values in Terraform state.

Required value formats:

- `database_url`: `postgres://helmr_app:<application-password>@<postgres_endpoint>/helmr?sslmode=require`
- `setup_token`: high-entropy string used only in self-hosted mode; read it from Secrets Manager for first organization setup
- `worker_token_signing_key`, `auth_key`, `encryption_key`, `checkpoint_encryption_key`, `workspace_fencing_key`, `token_credential_key`: base64-encoded 32-byte keys
- `github_oauth_client_secret`: GitHub OAuth client secret

The helper script generates `worker_token_signing_key`, `auth_key`, `encryption_key`,
`workspace_fencing_key`, `token_credential_key`, `checkpoint_encryption_key`, and `setup_token` locally and writes them
directly to Secrets Manager:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `HELMR_DATABASE_URL` and `HELMR_GITHUB_OAUTH_CLIENT_SECRET` to populate external secrets in the same run. The
helper uses `tofu` by default; set `TOFU=terraform` when using Terraform. It initializes missing
values only and never replaces an existing value. Rotate a Worker Group enrollment token from
Admin and propagate it to Workers explicitly. Online rotation of root keys is not supported.

The RDS-generated master credential is used only by the database bootstrap task. Control Plane,
Dispatcher, and migrations use the static application credential in `database_url`.

## Email

Email delivery is disabled by default. For Resend, configure:

```hcl
email_provider = "resend"
email_from     = "Helmr <noreply@example.com>"
```

After applying, populate the emitted `secret_arns.resend_api_key` Secrets Manager secret with the
Resend API key before starting the Control Plane service.

## Bootstrap the database and run migrations

After secrets are populated, run the database bootstrap task once. It idempotently creates the
non-administrative application role from `database_url` by using the RDS master credential inside
the VPC:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw database_bootstrap_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    --arg assignPublicIp "$([ "$(tofu output -raw controlplane_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:$assignPublicIp}}')"
```

Wait for that task to succeed, then run the migration task before enabling the services:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    --arg assignPublicIp "$([ "$(tofu output -raw controlplane_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:$assignPublicIp}}')"
```

Then set `create_controlplane_service=true` and apply again. This starts separate `helmr-controlplane` and
`helmr-dispatcher` ECS services using `controlplane_desired_count` and `dispatcher_desired_count`.

## Optional Worker Smoke

Workers are intentionally disabled by default. To create one nested-virtualization smoke worker,
set:

```hcl
enable_nat_gateway                  = true
create_worker                       = true
worker_instance_type                = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_min_size                     = 1
worker_max_size                     = 1
worker_root_volume_size_gb          = 120
worker_disk_mib                     = null
```

When workers are enabled, `certificate_arn` and a Worker Control Plane DNS name are required. The stack
derives the worker Control Plane URL from `public_url` for direct ALB mode or from
`cloudfront_origin_domain_name` for CloudFront mode, then resolves that hostname to an internal ALB
inside the VPC.

The official worker AMI is resolved from `helmr_version` and `aws_region`. Set `worker_ami_id` only
for custom builds; custom AMIs must satisfy the `modules/worker` contract: Firecracker, jailer,
`ip`, `nft`, certified guest boot artifacts, AWS CLI,
`worker`, and the executable `/usr/local/sbin/helmr-prepare-root` matching
`modules/worker-image/templates/prepare-root.sh` installed. Keep NAT enabled
while a worker is running or draining because workers run in private subnets. Workers are
filesystem-first: the root EBS volume carries build/cache/runtime data, and `worker_disk_mib` can
override the disk capacity advertised to the Control Plane.

The stack derives each Worker Pool generation name from the complete immutable
supply definition: Worker module/user-data contract, resolved AMI,
instance/runtime class, network/store/cache policy, root-volume shape, and
advertised capacity shape. Changing one of those sealed inputs
creates a new Pool name; changing only ASG minimum or maximum size does not.
Each Pool name keys a distinct Auto Scaling Group and launch template. Before
changing an immutable input, copy the old entry from
`worker_generation_definitions` into `retained_worker_generations` with
`min_size = 0`. The exported entry includes the realized user data, IAM
documents, SSM choice, and exact launch-template version, so the retained ASG
does not follow a newer template. Remove a retained entry only after Product
restore authority no longer references that Pool and its exact drain-to-
`termination_ready` retirement has completed. Control Plane does not
authenticate or allowlist the AMI; `worker_generation_bindings` records the
exact Product Pool to provider binding.

Deployment infrastructure owns desired capacity for execution groups.
Terraform retains the ASG min/max guardrails, and equal min/max values provide
fixed capacity. Worker capacity and disk/cache partitions must be explicit when
workers are created. Demand observations may guide scale-out, but scale-in must
use the exact claim-fenced drain contract.

## Destroy

```sh
tofu destroy
```

## Direct ALB Endpoint

For a direct ALB HTTPS endpoint instead of CloudFront, set:

```hcl
enable_cloudfront = false
public_url        = "https://helmr.example.com"
certificate_arn   = "arn:aws:acm:..."
```

Use an ACM certificate in the same region as the ALB.

For CloudFront, set `enable_cloudfront=true` and set `cloudfront_origin_domain_name` to a separate
DNS name, such as `origin.helmr.example.com`, that resolves to the public ALB and is covered by
`certificate_arn`. Do not reuse the CloudFront viewer hostname as the origin.
