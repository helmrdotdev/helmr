# Helmr AWS Standard Example

This example is a cost-conscious production baseline for deploying Helmr into a single customer AWS
account with OpenTofu or Terraform. It composes the shared AWS modules in `../modules` and keeps
secret values out of state.

## Baseline

- Two availability zone VPC with public subnets for the ALB and private subnets for control,
  migration, Postgres, and worker resources
- Single NAT Gateway enabled for private subnet egress. This reduces cost, but private control,
  migration, and worker egress depend on one NAT Gateway and may incur cross-AZ data processing.
- Control-plane ECS/Fargate tasks run in private subnets with no public IPs
- Internet-facing ALB terminates HTTPS with a customer-managed ACM certificate
- CloudFront disabled by default; `enable_cloudfront` is exposed for deployments that explicitly
  want the module-provided distribution. When enabled, CloudFront connects to the ALB over HTTPS
  using a separate origin DNS name and ACM certificate.
- `helmr-control` desired count defaults to 2; `helmr-dispatcher` desired count defaults to 1
- Cluster-mode disabled ElastiCache Valkey/Redis provides `HELMR_REDIS_URL` for dispatch; two
  nodes are used by default for automatic failover
- Worker Auto Scaling resources are optional and private-subnet ready, with zero default capacity
- RDS defaults to deletion protection, automated backups, final snapshots, encrypted storage, and
  longer recovery windows

## Initialize

Copy `terraform.tfvars.example` to a local tfvars file and fill in the non-secret values:

```sh
cp terraform.tfvars.example standard.tfvars
tofu init
tofu apply -var-file=standard.tfvars
```

This example intentionally has no backend block. Add your own backend configuration in the
deployment copy if you need shared remote state. This repository does not commit a
`.terraform.lock.hcl` because Terraform and OpenTofu write different provider hostnames. Commit the
generated lock file in your deployment repository.

## Deployment Flow

The first apply should normally keep `create_control_service=false`. That creates the VPC, ALB,
RDS, CAS bucket, KMS key, empty Secrets Manager containers, and release-backed task definitions
without starting tasks that need populated secrets.

Populate the emitted Secrets Manager secrets out-of-band, run the database migration task, then set
`create_control_service=true` and apply again. The stack creates empty secret containers; it does
not generate or store Helmr internal secret values in Terraform state. This starts separate
`helmr-control` and `helmr-dispatcher` ECS services using `control_desired_count` and
`dispatcher_desired_count`. The official control image is resolved from `helmr_version`; set
`control_image` only for digest-pinned custom builds.

Required secret value formats:

- `database_url`: Postgres connection URL for the `helmr` database with SSL required
- `worker_token_signing_key`, `auth_secret`, `worker_registration_token`, `setup_token`: high-entropy strings
- `setup_token`: read it from Secrets Manager for first organization setup
- `secret_encryption_key`, `checkpoint_encryption_key`: base64-encoded 32-byte keys
- `github_app_private_key`: raw GitHub App private key PEM
- `github_app_webhook_secret`, `github_app_client_secret`: GitHub App values

The helper script generates `worker_token_signing_key`, `auth_secret`, `secret_encryption_key`,
`checkpoint_encryption_key`, `worker_registration_token`, and `setup_token` locally and writes them
directly to Secrets Manager:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `HELMR_DATABASE_URL`, `HELMR_GITHUB_APP_PRIVATE_KEY_FILE` or
`HELMR_GITHUB_APP_PRIVATE_KEY`, `HELMR_GITHUB_APP_WEBHOOK_SECRET`, and
`HELMR_GITHUB_APP_CLIENT_SECRET` to populate the external application secrets in the same run. The
helper uses `tofu` by default; set `TOFU=terraform` when using Terraform. Set
`OVERWRITE_SECRETS=1` only when intentionally rotating values.

Run migrations after secrets are populated:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw control_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json control_task_subnet_ids)" \
    --arg sg "$(tofu output -raw control_security_group_id)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:"DISABLED"}}')"
```

## DNS and HTTPS

Create or validate an ACM certificate for `public_url` in the same region as the ALB. Point the
customer DNS name at `control_load_balancer_dns_name`, then enable the control and dispatcher
services after
secrets and migrations are ready.

If `enable_cloudfront=true`, use the `control_url` output for browser and CLI traffic, set
`cloudfront_origin_domain_name` to a separate DNS name that resolves to the public ALB, and keep
that name covered by `certificate_arn`. Workers use that same origin hostname privately inside the
VPC, so do not reuse the CloudFront viewer hostname as the origin.

## Workers

Worker resources are not created until `create_worker=true`. The official worker AMI is resolved
from `helmr_version` and `aws_region`; set `worker_ami_id` only for custom builds. Increase
`worker_desired_capacity` and `worker_min_size` when you are ready to launch hosts.

Workers launch in private subnets, use SSM Session Manager by default, and do not require inbound
SSH rules. The default worker instance type is a metal host for production isolation; nested
virtualization remains available for supported instance families when explicitly enabled. Workers
are filesystem-first: size the root EBS volume for build/cache/runtime data, and set
`worker_disk_mib` only when the advertised filesystem capacity should differ from auto-detection.
