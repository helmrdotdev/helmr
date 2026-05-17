# Helmr AWS Quickstart

This example deploys a low-cost Helmr self-hosting baseline for evaluation, PoC, or a startup
environment. It uses the shared AWS modules for network, control plane, and optional workers.

Defaults:

- CloudFront is enabled with the AWS-managed `*.cloudfront.net` domain and default certificate.
- NAT Gateway is disabled.
- Control and migration Fargate tasks run with public IPs, while inbound task traffic still comes
  only from the load balancer security group.
- The control service desired count is `1`.
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
`helmr_version`, GitHub App metadata, and `bootstrap_owner_email` for the initial owner. Do not put
GitHub private keys, webhook secrets, client secrets, database URLs, or Helmr signing keys in
tfvars.

Initialize and apply:

```sh
tofu init
tofu apply
```

The first apply should usually keep `create_control_service=false`. It creates infrastructure,
resolves the official release artifacts, generates Helmr internal secrets, and creates the
migration task definition without trying to start a service that cannot yet read externally
populated secrets.

This example intentionally has no backend block. Add your own backend configuration in the
deployment copy if you need shared remote state.

## Populate Secrets

After the first apply, populate the external Secrets Manager ARNs from
`tofu output -json secret_arns`. Required value formats:

- `database_url`: `postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require`
- `github_app_private_key`: raw GitHub App private key PEM
- `github_app_webhook_secret`, `github_app_client_secret`: GitHub App values

`worker_token_signing_key`, `auth_secret`, `secret_encryption_key`,
`checkpoint_encryption_key`, and `worker_pool_registration_token` are generated and stored by
OpenTofu/Terraform.

The RDS-generated master password ARN is available as `database_master_user_secret_arn`.

## Run Migrations

Run the migration task after secrets are populated and before enabling the control service:

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

Then set `create_control_service=true` and apply again. The default `control_url` output is the
CloudFront URL.

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
```

The official worker AMI is resolved from `helmr_version` and `aws_region`. Set `worker_ami_id` only
for custom builds; custom AMIs must satisfy the `modules/worker` contract: Firecracker, jailer,
BuildKit, CNI plugins, guest boot artifacts, AWS CLI, and `helmr-worker` installed. Keep NAT enabled
while a worker is running or draining because workers run in private subnets.

## Destroy

```sh
tofu destroy
```

## Direct Custom Domain

For a direct ALB HTTPS endpoint instead of the CloudFront default domain, set:

```hcl
enable_cloudfront = false
public_url        = "https://helmr.example.com"
certificate_arn   = "arn:aws:acm:..."
```

Use an ACM certificate in the same region as the ALB.
