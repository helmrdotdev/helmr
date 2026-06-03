# Helmr AWS Quickstart

This example deploys a low-cost Helmr self-hosting baseline for evaluation, PoC, or a startup
environment. It uses the shared AWS modules for network, control plane, and optional workers.

Defaults:

- CloudFront is disabled by default. When enabled, it uses the AWS-managed `*.cloudfront.net`
  viewer domain and a separate HTTPS ALB origin DNS name.
- NAT Gateway is disabled.
- Control and migration Fargate tasks run with public IPs, while inbound task traffic still comes
  only from the load balancer security group.
- The `helmr-control` service desired count is `1`.
- The `helmr-dispatcher` service desired count is `1`.
- A single-node, cluster-mode disabled ElastiCache Valkey/Redis queue is provisioned for
  `HELMR_REDIS_URL`.
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

The first apply should usually keep `create_control_service=false`. It creates infrastructure,
resolves the official release artifacts, creates empty Secrets Manager containers, and creates the
migration task definition without trying to start a service that cannot yet read populated secrets.

This example intentionally has no backend block. Add your own backend configuration in the
deployment copy if you need shared remote state.

## Populate Secrets

After the first apply, populate the Secrets Manager ARNs from `tofu output -json secret_arns`.
The stack creates empty secret containers; it does not generate or store Helmr internal secret
values in Terraform state.

Required value formats:

- `database_url`: `postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require`
- `worker_token_signing_key`, `auth_secret`, `worker_bootstrap_token`, `setup_token`: high-entropy strings
- `setup_token`: read it from Secrets Manager for first organization setup
- `secret_encryption_key`, `checkpoint_encryption_key`: base64-encoded 32-byte keys
- `github_oauth_client_secret`: GitHub OAuth client secret

The helper script generates `worker_token_signing_key`, `auth_secret`, `secret_encryption_key`,
`checkpoint_encryption_key`, `worker_bootstrap_token`, and `setup_token` locally and writes them
directly to Secrets Manager:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `HELMR_DATABASE_URL` and `HELMR_GITHUB_OAUTH_CLIENT_SECRET` to populate external secrets in the same run. The
helper uses `tofu` by default; set `TOFU=terraform` when using Terraform. Set
`OVERWRITE_SECRETS=1` only when intentionally rotating values.

The RDS-generated master password ARN is available as `database_master_user_secret_arn`.

## Email

Email delivery is disabled by default. For Resend, configure:

```hcl
email_provider = "resend"
email_from     = "Helmr <noreply@example.com>"
```

After applying, populate the emitted `secret_arns.resend_api_key` Secrets Manager secret with the
Resend API key before starting the control service.

## Run Migrations

Run the migration task after secrets are populated and before enabling the control and dispatcher
services:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw control_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json control_task_subnet_ids)" \
    --arg sg "$(tofu output -raw control_security_group_id)" \
    --arg assignPublicIp "$([ "$(tofu output -raw control_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assignPublicIp}}')"
```

Then set `create_control_service=true` and apply again. This starts separate `helmr-control` and
`helmr-dispatcher` ECS services using `control_desired_count` and `dispatcher_desired_count`.

## Optional Worker Smoke

Workers are intentionally disabled by default. To create one nested-virtualization smoke worker,
set:

```hcl
enable_nat_gateway                  = true
create_worker                       = true
worker_instance_type                = "c8i.xlarge"
worker_enable_nested_virtualization = true
worker_desired_capacity             = 1
worker_min_size                     = 1
worker_max_size                     = 1
worker_root_volume_size_gb          = 120
worker_disk_mib                     = null
```

When workers are enabled, `certificate_arn` and a worker control DNS name are required. The stack
derives the worker control URL from `public_url` for direct ALB mode or from
`cloudfront_origin_domain_name` for CloudFront mode, then resolves that hostname to an internal ALB
inside the VPC.

The official worker AMI is resolved from `helmr_version` and `aws_region`. Set `worker_ami_id` only
for custom builds; custom AMIs must satisfy the `modules/worker` contract: Firecracker, jailer,
BuildKit, CNI plugins, guest boot artifacts, AWS CLI, and `helmr-worker` installed. Keep NAT enabled
while a worker is running or draining because workers run in private subnets. Workers are
filesystem-first: the root EBS volume carries build/cache/runtime data, and `worker_disk_mib` can
override the disk capacity advertised to the control plane.

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
