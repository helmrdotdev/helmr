---
title: AWS production
description: Start a customer environment from the standard AWS baseline and identify the production work that remains operator-owned.
sidebarLabel: AWS production
---

# AWS production

`infra/aws/standard` is a cost-conscious production baseline for one customer AWS account. It is a starting composition, not a claim that every production requirement is complete.

The baseline uses two availability zones, private Control Plane, dispatcher, migration, database, and worker subnets, an internet-facing HTTPS ALB, one NAT gateway, two Control Plane tasks, one dispatcher task, a two-node Valkey/Redis replication group, Multi-AZ RDS, deletion protection, automated backups, final snapshots, encrypted storage, and longer retention windows. Workers remain disabled by default.

The single NAT gateway reduces cost but is an egress dependency and can incur cross-AZ data processing. ClickHouse, remote state, credentials, alerting, capacity policy, backup restore testing, drift management, disaster recovery, and any stronger availability target remain operator-owned.

## Configure the baseline

From a deployment copy:

```sh
cd infra/aws/standard
cp terraform.tfvars.example standard.tfvars
```

Fill every required value in [requirements](/docs/self-hosting/requirements/). Set `public_url` to your customer HTTPS origin and provide an ACM certificate in the same region as the ALB:

```hcl
public_url     = "https://helmr.example.com"
certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/example"

create_controlplane_service = false
create_worker               = false
```

Initialize and apply:

```sh
tofu init
tofu apply -var-file=standard.tfvars
```

The example has no backend block and does not commit a provider lock file. Add your deployment's backend and commit the generated lock file in that deployment repository.

Point `public_url` DNS at `controlplane_load_balancer_dns_name`. If CloudFront is enabled, use a distinct ALB origin DNS name covered by the certificate and keep browser and CLI traffic on the emitted `controlplane_url`.

## Production readiness work

Before serving customer traffic, at minimum:

- Define ownership for Terraform state, credentials, drift, on-call response, scaling, and cost guardrails.
- Provision and validate the ClickHouse endpoint and its network path.
- Choose database, Redis, object-store, and log retention policies that meet your recovery targets; test restoration into a separate environment.
- Validate the single-NAT tradeoff or replace it in your deployment composition.
- Configure monitoring for ECS service health, `/readyz`, RDS, Redis/Valkey, ClickHouse, queues, and worker capacity.
- Populate secrets, run database bootstrap and migrations, and verify [authentication](/docs/self-hosting/authentication/).
- Size and test immutable execution [Worker](/docs/self-hosting/workers/) Pool generations.
- Rehearse the [upgrade](/docs/self-hosting/upgrades/) and worker-drain sequence in a non-production environment.

Only then enable the services using the [Control Plane](/docs/self-hosting/control-plane/) procedure.
