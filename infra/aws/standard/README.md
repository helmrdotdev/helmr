# Helmr AWS Standard Example

This example is a cost-conscious production baseline for deploying Helmr into a single customer AWS
account with OpenTofu or Terraform. It composes the shared AWS modules in `../modules` and keeps
secret values out of state.

The default Control Plane task security group allows every outbound protocol to
every IPv4 destination through NAT. This supports operator-selected integrations,
including ClickHouse and SMTP. See [Restrict platform egress](#restrict-platform-egress)
for an opt-in example.

## Baseline

- Two availability zone VPC with public subnets for the ALB and private subnets for Control Plane,
  migration, Postgres, and worker resources
- Single NAT Gateway enabled for private subnet egress. This reduces cost, but private Control Plane,
  migration, and worker egress depend on one NAT Gateway and may incur cross-AZ data processing.
- Control Plane ECS/Fargate tasks run in private subnets with no public IPs
- Internet-facing ALB terminates HTTPS with a customer-managed ACM certificate
- CloudFront disabled by default; `enable_cloudfront` is exposed for deployments that explicitly
  want the module-provided distribution. When enabled, CloudFront connects to the ALB over HTTPS
  using a separate origin DNS name and ACM certificate.
- `helmr-controlplane` desired count defaults to 2; `helmr-dispatcher` desired count defaults to 1
- Worker Auto Scaling resources are optional and private-subnet ready, with zero default capacity
- RDS defaults to deletion protection, automated backups, final snapshots, encrypted storage, and
  longer recovery windows

## Restrict platform egress

This example assumes private connectivity to ClickHouse, private-DNS AWS
interface endpoints and an operator-managed CONNECT proxy. Install the endpoint
interfaces for ECR API/DKR, Secrets Manager and CloudWatch Logs, plus KMS when
used. Mirror the released Control Plane image into a private regional ECR
repository, preserving the release manifest digest. Set both the mirrored URI
and matching repository ARN so the Fargate execution roles can pull the image.
Use the Standard VPC's regional S3 gateway endpoint for artifact and ECR layer
downloads. The proxy must allow the external GitHub, Slack and email
providers you enable. SMTP uses a direct connection to your relay. Dispatcher
also needs direct HTTPS to its configured Control Plane URL. Supply a maintained
IPv4 prefix list that covers the actual frontend addresses reached from the task
subnets (including CloudFront if enabled). A public ALB's current DNS answers
are not a permanent allowlist. Confirm how the list tracks address changes; use
a different controlled origin path if you cannot maintain it.
These are prerequisites; the example creates only task egress rules.

1. Copy [restricted-egress.tf.example](restricted-egress.tf.example) to
   `restricted-egress.tf` in your Standard root. Inside the **existing**
   `module "controlplane"` block in `main.tf`, add both arguments:

   ```hcl
   restrict_platform_egress = true
   outbound_proxy_url       = "http://${var.restricted_egress.proxy_hostname}:3128"
   ```

2. Set the new input in your tfvars. Replace the illustrative values with
   actual reachable destination security groups and the port in `clickhouse_url`:

   ```hcl
   controlplane_image                = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr/control-plane@sha256:<release-manifest-digest>"
   controlplane_image_repository_arn = "arn:aws:ecr:us-east-1:111122223333:repository/helmr/control-plane"
   restricted_egress = {
     origin_prefix_list_id = "pl-00000000000000001"
     aws_endpoint_sg_id = "sg-00000000000000001"
     clickhouse_sg_id   = "sg-00000000000000002"
     clickhouse_port    = 443
     proxy_sg_id        = "sg-00000000000000003"
     proxy_hostname     = "proxy.platform.example"
     # For email_provider = "smtp", also set smtp_sg_id and smtp_port to
     # the relay's security group and the port in smtp_addr.
   }
   ```

3. Permit ingress on the corresponding destination groups from the platform
   task group. Verify routes, DNS and endpoint policies. Check the external URL
   of any additional enabled integration. For public endpoints without a
   referenceable security group, supply a controlled route and destination rule
   suited to that endpoint. All security groups on an ENI contribute permissions;
   inspect `additional_controlplane_security_group_ids` for broader grants.
4. Review the plan. The general platform egress rule must disappear. Expect
   TCP 5432 to RDS, 443 to AWS endpoints, the S3 and origin prefix lists, the configured
   ClickHouse port, 3128 to the proxy and the SMTP relay rule only when enabled.
   In a disposable deployment, check image pull, secret retrieval, bootstrap
   and migration exit codes, both ECS services reaching steady state, a platform object
   read and a CAS write/read, and a request through each enabled integration.
   Confirm an unlisted destination is unreachable directly from the task network.

This example changes platform tasks only. Worker, guest and build networking
retain their own policies.

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

The first apply should normally keep `create_controlplane_service=false`. That creates the VPC, ALB,
RDS, CAS bucket, KMS key, empty Secrets Manager containers, and release-backed task definitions
without starting tasks that need populated secrets.

Populate the emitted Secrets Manager secrets out-of-band, run the database bootstrap task followed
by the migration task, then set `create_controlplane_service=true` and apply again. The stack creates
empty secret containers; it does not generate or store Helmr internal secret values in Terraform
state. This starts separate
`helmr-controlplane` and `helmr-dispatcher` ECS services using `controlplane_desired_count` and
`dispatcher_desired_count`. Set `controlplane_image` to the digest-pinned image in the verified
common release index; common releases do not publish the resolver's default AWS manifest.

Required secret value formats:

- `database_url`: Postgres connection URL for the `helmr_app` application role and `helmr` database with SSL required
- `worker_host_credential_signing_key`, `auth_key`, `encryption_key`, `computer_fencing_key`: base64-encoded 32-byte keys
- `github_oauth_client_secret`: GitHub OAuth client secret

The helper script generates `worker_host_credential_signing_key`, `auth_key`, `encryption_key`,
and `computer_fencing_key` locally and writes them
directly to Secrets Manager:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
```

Set `HELMR_DATABASE_URL` and `HELMR_GITHUB_OAUTH_CLIENT_SECRET` to populate external secrets in the same run. The
helper uses `tofu` by default; set `TOFU=terraform` when using Terraform. It initializes missing
values only and never replaces an existing value. Rotate a Worker Group enrollment token from
Admin and propagate it to Workers explicitly. Online rotation of root keys is not supported.

After secrets are populated, run the database bootstrap task and wait for it to succeed:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw database_bootstrap_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:"DISABLED"}}')"
```

The RDS master credential is used only by this task. Control Plane, Dispatcher, and migrations use
the static application credential in `database_url`. Then run migrations:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:"DISABLED"}}')"
```

## Email

Email delivery is disabled by default. For Resend, configure:

```hcl
email_provider = "resend"
email_from     = "Helmr <noreply@example.com>"
```

After applying, populate the emitted `secret_arns.resend_api_key` Secrets Manager secret with the
Resend API key before starting the Control Plane service.

## DNS and HTTPS

Create or validate an ACM certificate for `app_origin` in the same region as the ALB. Point the
customer DNS name at `controlplane_load_balancer_dns_name`, then enable the Control Plane and dispatcher
services after
secrets and migrations are ready.

If `enable_cloudfront=true`, use the `controlplane_url` output for browser and CLI traffic, set
`cloudfront_origin_domain_name` to a separate DNS name that resolves to the public ALB, and keep
that name covered by `certificate_arn`. Workers use that same origin hostname privately inside the
VPC, so do not reuse the CloudFront viewer hostname as the origin.

## Workers

Worker resources are not created until `create_worker=true`. Use the verified public release index and prepare a Worker AMI from its exact
host/runtime bundles, then supply `worker_ami_id` and digest-pinned
`controlplane_image`. Common releases do not publish AWS AMIs or an AWS manifest. Set an explicit
`worker_count` whenever `create_worker=true`, including `0` for an inert generation.

Workers launch in private subnets, use SSM Session Manager by default, and do not require inbound
SSH rules. The default worker instance type is a metal host for production isolation; nested
virtualization remains available for supported instance families when explicitly enabled. Workers
are filesystem-first: size the root EBS volume for runtime/cache data, and set
`worker_disk_mib` only when the advertised filesystem capacity should differ from auto-detection.

The stack derives each Worker Pool generation name from the complete immutable
supply definition: Worker module/user-data contract,
resolved AMI, instance/runtime class, network/store/cache policy,
root-volume shape, roles, and advertised capacity shape. Changing one of those
sealed inputs creates a new Pool name; changing only `worker_count`
does not. `worker_generation_key` also creates a fresh binding for explicitly qualified
same-build recovery; retain its old definition before changing the key.
Each Pool name keys a distinct Auto Scaling Group and launch
template. Before changing an immutable input, copy the old entry from
`worker_generation_definitions` into `retained_worker_generations` unchanged,
including its current `count`. Set the new generation's `worker_count = 0`
during preparation. Do not reduce the retained source count until the full-stop
gate in the [maintenance procedure](../../../packages/web/src/content/docs/self-hosting/upgrades.md)
has passed. The exported entry includes the realized user data, IAM
documents, SSM choice, and exact launch-template version, so the retained ASG
does not follow a newer template. Remove a retained entry only after the separate fenced Product Pool retirement
returns `status = disabled`, with no pending launches and exact provider absence
confirmed again before cleanup. Host `termination_ready` alone does not retire a Pool. Control Plane does not
authenticate or allowlist the AMI; `worker_generation_bindings` records the
exact Product Pool to provider binding.

The reference stack owns fixed capacity: each generation has explicit desired
and maximum counts, with minimum zero so an exact lost host can be removed
without replenishing capacity during maintenance. Supply increases use the same qualified build and
profile. Every reduction, including zero, and every worker update uses the
[full-stop maintenance procedure](../../../packages/web/src/content/docs/self-hosting/upgrades.md).
There is no automatic instance refresh. Keep Control Plane, dispatcher, network,
storage and keys available throughout drain. Explicit worker capacity and
disk/cache partitions remain required when workers are created.

Enroll a separate deployment operator through the protected local command in the
[authentication guide](../../../packages/web/src/content/docs/self-hosting/authentication.md).
Keep Capacity credentials separate from operator login sessions.

For native maintenance requests, set `capacity_token_secret_arn` to an existing
Secrets Manager secret containing the deployment Capacity token. For a customer
KMS key, also set `capacity_token_kms_key_arn`. These inputs contain ARNs, never
the credential value; only the Control Plane service receives the token.

## Deployment recovery

`enable_deployment_rollback` defaults to `true` for both ECS services. Automatic
recovery can restore the preceding task definition only when its code remains
compatible with current data; it does not restore the database or verify schema
compatibility. Apply `enable_deployment_rollback=false` before an incompatible
migration or reset and recover by rolling forward. Run migrations from the exact
selected image before enabling or updating services.

ClickHouse uses explicit reader, ingester and migration usernames and separate
Secrets Manager passwords. In `clickhouse_access_mode = "external"`, provision
these accounts separately before deployment. `bootstrap` mode also requires an
administrator credential and exposes `clickhouse_bootstrap_task_definition_arn`;
run that task successfully before migrations. The task uses the Control Plane
network and its existing image. See [ClickHouse access](../clickhouse-access.md)
for grants, bootstrap behavior and credential rotation.

Diagnostic ingestion and export limits must be explicitly supplied through
`controlplane_environment` and `dispatcher_environment`; see the shared
[diagnostic process limits](../README.md#diagnostic-process-limits). The example
values are development starting points and require operator review.


Upload capacities and deadlines are explicit common Product settings; see
[deployment upload capacity](../README.md#deployment-upload-capacity) before
choosing values for the selected task size and ingress.
