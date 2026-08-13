---
title: Requirements
description: Prepare the accounts, tools, release inputs, data services, and network policy required for self-hosting.
---

# Requirements

Prepare these inputs before applying either AWS composition.

## Accounts and tools

- An AWS account and one primary AWS region.
- AWS credentials allowed to create the VPC, load balancer, ECS, RDS, ElastiCache, S3, KMS, Secrets Manager, IAM, EC2, Auto Scaling, and optional CloudFront resources in the composition you select.
- OpenTofu or Terraform, plus the AWS CLI, `jq`, OpenSSL, and curl. The repository's Nix development shell provides the pinned project toolchain.
- A GitHub OAuth app for browser login.
- A customer-owned ClickHouse service reachable from Control Plane, dispatcher, and migration tasks over HTTPS.

## Required deployment inputs

Both `infra/aws/quickstart` and `infra/aws/standard` require:

| Input | Purpose |
| --- | --- |
| `aws_region` | Selects regional infrastructure and the release's worker AMI. |
| `helmr_version` | Resolves the official AWS release manifest. |
| `region_id` | Identifies this Helmr region. |
| `worker_group_name` | Names the initial logical worker group. |
| `platform_store_uri` | Points to immutable Platform Artifact objects. |
| `platform_store_bucket_arn` | Authorizes access to the Platform Artifact bucket. |
| `platform_store_kms_key_arn` | Authorizes decryption of Platform Artifacts. |
| `clickhouse_url` | HTTPS endpoint for historical telemetry. |
| `github_oauth_client_id` | Non-secret OAuth application client ID. |
| `worker_network_blocked_ipv4_cidrs` | Deployment-owned deny set that must wholly cover the execution VPC prefix. |

The Platform Artifact values come from an operator-managed foundation built with `infra/aws/modules/bootstrap` or an equivalent deployment. The example compositions do not create that foundation for you.

## Release artifacts

By default, `helmr_version` resolves:

```text
https://github.com/helmrdotdev/helmr/releases/download/<helmr_version>/aws-artifacts.json
```

The manifest must contain a digest-pinned Control Plane image and, when workers are enabled, an AMI for `aws_region`. Custom Control Plane images must also use `@sha256:<digest>`; custom worker overrides must be a valid AMI ID.

## Worker prerequisites

Workers additionally need:

- Private-subnet outbound access to the Control Plane, S3, ECR, AWS APIs, registries, and any external services tasks call.
- KVM-capable EC2 capacity. The evaluation profile supports explicitly enabled nested virtualization on supported families; the production profile defaults to a metal instance.
- A worker AMI containing `helmr-worker`, Firecracker, jailer, `ip`, `nft`, AWS CLI v2, curl, the systemd unit, and certified guest boot artifacts.
- Explicit host, VM, cache, disk, and execution-slot capacity sized for the workload.
- SSM access for maintenance unless you supply an alternative. The module does not open SSH by default.

Review [workers](/docs/self-hosting/workers/) before creating capacity.
