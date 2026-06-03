# Helmr AWS Dev Stack

This stack is the deployable AWS development and full-run smoke environment for Helmr.

It creates Secrets Manager containers for Helmr internal and application secrets, but it does not
store secret values in OpenTofu/Terraform state. Populate those secrets before starting
control-plane services.

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
  -var="github_oauth_client_id=Iv1..."
```

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

Required secret value formats:

- `database_url`: `postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require`
- `worker_token_signing_key`, `auth_secret`, `worker_bootstrap_token`, `setup_token`: high-entropy strings
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
    --arg sg "$(tofu output -raw control_security_group_id)" \
    --arg assignPublicIp "$([ "$(tofu output -raw control_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assignPublicIp}}')"
```

Worker resources are not created until `create_worker=true`. Build and publish a worker AMI that
satisfies the worker module contract, set `worker_ami_id`, ensure `certificate_arn` and the worker
control DNS name are configured, then increase `worker_desired_capacity` and `worker_min_size`.
Workers are filesystem-first Firecracker hosts; size
`worker_root_volume_size_gb` for build/cache/runtime data and use `worker_disk_mib` only to override
advertised filesystem capacity.

For ephemeral full-run smoke testing, use `full-run-smoke.tfvars.example` as the starting point. It
keeps capacity at one worker, enables SSM access, and uses EC2 nested virtualization on `c8i.xlarge`
instead of requiring a bare-metal worker instance. Destroy the stack after the smoke run; it still
creates RDS, NAT, ALB, and EC2 resources.
When scaling down from an active worker, first apply worker desired/min capacity
zero while NAT is still present so lifecycle drain can finish, then apply control
mode to remove NAT.
