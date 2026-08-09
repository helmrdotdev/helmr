---
title: AWS evaluation
description: Deploy the low-cost quickstart profile for evaluation and proof-of-concept use.
sidebarLabel: AWS evaluation
---

# AWS evaluation

`infra/aws/quickstart` is a low-cost evaluation and proof-of-concept composition. It is not the production baseline.

Its defaults include one Control Plane task, one dispatcher task, a single-node Valkey/Redis event stream, a small RDS instance, short retention windows, no database deletion protection, no final snapshot, and no workers. NAT is disabled, while Control Plane and one-off Fargate tasks receive public IPs; their inbound traffic is still restricted to the load balancer security group.

## Configure the profile

From a deployment copy of the profile:

```sh
cd infra/aws/quickstart
cp terraform.tfvars.example terraform.tfvars
```

Fill every required value described in [requirements](/docs/self-hosting/requirements/). Keep secret values out of tfvars and state. For a direct HTTPS endpoint, configure:

```hcl
enable_cloudfront = false
public_url        = "https://helmr.example.com"
certificate_arn   = "arn:aws:acm:us-east-1:123456789012:certificate/example"
```

The ACM certificate must be in the ALB's region. If you enable CloudFront, give `cloudfront_origin_domain_name` a separate DNS name that resolves to the ALB and is covered by that certificate; do not reuse the CloudFront viewer hostname as its origin.

Keep services and workers off for the first apply:

```hcl
create_controlplane_service = false
create_worker               = false
```

Then initialize and apply:

```sh
tofu init
tofu apply
```

The example intentionally has no backend block. Add and operate a remote-state backend in your deployment repository if the evaluation needs shared state.

## Continue setup

After the first apply:

1. Record `controlplane_url`, `controlplane_load_balancer_dns_name`, `controlplane_cluster_name`, and `secret_arns`.
2. Complete [authentication](/docs/self-hosting/authentication/) and [secrets and data](/docs/self-hosting/secrets-and-data/).
3. Bootstrap the database, run migrations, and enable the [Control Plane](/docs/self-hosting/control-plane/).
4. To test code execution, enable NAT and add one supported nested-virtualization worker as described in [workers](/docs/self-hosting/workers/).

Evaluation defaults are intentionally easy to destroy and provide limited recovery. Do not rely on them for customer data or convert the stack incrementally into production.
