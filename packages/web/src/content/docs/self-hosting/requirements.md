---
title: Requirements
description: Accounts, tools, network, and release artifacts required before deployment.
section: Self-hosting
sidebarLabel: Requirements
order: 710
---

# Requirements

Prepare these before creating the AWS stack.

| Requirement | Notes |
| --- | --- |
| AWS account and region | Use one primary region for the control plane, database, object store, and workers. |
| AWS credentials | The deploying principal needs permission to create VPC, ECS, RDS, S3, Secrets Manager, IAM, ALB, CloudFront, Auto Scaling, and EC2 resources. |
| OpenTofu or Terraform | The repository examples are OpenTofu-compatible. Use the infra shell if you want the repo-pinned toolchain. |
| AWS CLI and `jq` | Needed for reading outputs, writing secret values, and running the migration task. |
| GitHub App | Required for repository access, OAuth login, setup flow, and webhook verification. |
| Helmr release version | AWS examples read control image and worker AMI metadata from the release artifact manifest. |
| Public URL | Use HTTPS for customer environments. Quickstart can use the generated CloudFront URL; production usually uses your own domain and ACM certificate. |

Workers have additional requirements because they run Firecracker guests:

- EC2 instance type with KVM support.
- Worker AMI that includes `helmr-worker`, Firecracker, jailer, CNI plugins, `tc-redirect-tap`, BuildKit, AWS CLI v2, curl, kernel, initramfs, and rootfs artifacts.
- Outbound access to the control plane, S3, ECR, AWS APIs, and GitHub.
- SSM access for maintenance. Do not expose SSH by default.

You can deploy the control plane first and add workers later.
