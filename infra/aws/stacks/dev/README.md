# Helmr AWS Dev Stack

This stack is the deployable AWS development and full-run smoke environment for Helmr.

It creates the Helmr internal secret values, including the shared worker pool registration token,
with OpenTofu/Terraform. Populate the remaining external Secrets Manager secrets before starting
control-plane services.

Initialize with an existing S3 backend:

```sh
terraform init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="region=<state-region>"
```

Then apply:

```sh
terraform apply \
  -var="aws_region=us-east-1" \
  -var="name=helmr-example" \
  -var="public_url=https://helmr.example.com" \
  -var="control_image=<account>.dkr.ecr.us-east-1.amazonaws.com/helmr-example/control:<version-or-digest>" \
  -var="certificate_arn=arn:aws:acm:..." \
  -var="github_app_id=123456" \
  -var="github_app_slug=helmr-dev" \
  -var="github_app_client_id=Iv1..."
```

This repository does not commit a `.terraform.lock.hcl` because Terraform and OpenTofu
write different provider hostnames. Commit the generated lock file in your deployment repository.

For domainless development smoke runs, set `enable_cloudfront=true` and leave
`certificate_arn=null`; the emitted `control_url` will use the generated
`https://*.cloudfront.net` URL.

The control-plane service runs on ECS/Fargate behind an ALB. The stack keeps
`create_control_service=false` by default so first apply can create infrastructure without trying
to pull an image or inject empty external secrets. Populate the emitted external Secrets Manager
secrets, push an immutable control image to `control_ecr_repository_url`, run the emitted migration
task definition once, then set
`create_control_service=true`.

The dev stack defaults to low-cost control mode: no NAT Gateway, one control
task, short log/object retention, and public IPs for control/migration Fargate
tasks. The task security group still only permits inbound traffic from the ALB.
Enable NAT Gateway only for run mode or production-like private egress.

Required secret value formats:

- `database_url`: `postgres://helmr:<password>@<postgres_endpoint>/helmr?sslmode=require`
- `github_app_private_key`: raw GitHub App private key PEM
- `github_app_webhook_secret`, `github_app_client_secret`: GitHub App values

`worker_token_signing_key`, `auth_secret`, `secret_encryption_key`,
`checkpoint_encryption_key`, and `worker_pool_registration_token` are generated and stored by
OpenTofu/Terraform.

Run migrations after secrets are populated:

```sh
aws ecs run-task \
  --cluster "$(terraform output -raw control_cluster_name)" \
  --task-definition "$(terraform output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(terraform output -json control_task_subnet_ids)" \
    --arg sg "$(terraform output -raw control_security_group_id)" \
    --arg assignPublicIp "$([ "$(terraform output -raw control_assign_public_ip)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:[$sg],assignPublicIp:$assignPublicIp}}')"
```

Worker resources are not created until `create_worker=true`. Build and publish a worker AMI that
satisfies the worker module contract, set `worker_ami_id`, then increase `worker_desired_capacity`
and `worker_min_size`.

For ephemeral full-run smoke testing, use `full-run-smoke.tfvars.example` as the starting point. It
keeps capacity at one worker, enables SSM access, and uses EC2 nested virtualization on `c8i.xlarge`
instead of requiring a bare-metal worker host. Destroy the stack after the smoke run; it still
creates RDS, NAT, ALB, and EC2 resources.
When scaling down from an active worker, first apply worker desired/min capacity
zero while NAT is still present so lifecycle drain can finish, then apply control
mode to remove NAT.
