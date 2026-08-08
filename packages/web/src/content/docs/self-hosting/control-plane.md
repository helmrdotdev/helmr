---
title: Control Plane
description: Bootstrap the database, run migrations, start Control Plane and dispatcher services, and verify readiness.
---

# Control Plane

The AWS compositions create task definitions before they create long-running services. This keeps workloads that require secrets and a current schema from starting during the first apply.

## Bootstrap the database

After populating secrets, run the database bootstrap task once. It idempotently creates the non-administrative `helmr_app` role from `database_url`, using the RDS-managed master credential inside the VPC.

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw database_bootstrap_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    --arg assignPublicIp "$([ "$(tofu output -raw controlplane_assign_public_ip 2>/dev/null || printf false)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:$assignPublicIp}}')"
```

Wait for the task to stop and confirm its container exit code is zero. Do not infer success from `aws ecs run-task` returning successfully.

## Run migrations

Use the same network configuration with `migration_task_definition_arn`:

```sh
aws ecs run-task \
  --cluster "$(tofu output -raw controlplane_cluster_name)" \
  --task-definition "$(tofu output -raw migration_task_definition_arn)" \
  --launch-type FARGATE \
  --network-configuration "$(jq -cn \
    --argjson subnets "$(tofu output -json controlplane_task_subnet_ids)" \
    --argjson securityGroups "$(tofu output -json controlplane_task_security_group_ids)" \
    --arg assignPublicIp "$([ "$(tofu output -raw controlplane_assign_public_ip 2>/dev/null || printf false)" = "true" ] && printf ENABLED || printf DISABLED)" \
    '{awsvpcConfiguration:{subnets:$subnets,securityGroups:$securityGroups,assignPublicIp:$assignPublicIp}}')"
```

Again, wait for a zero exit code. Run migrations for the exact Control Plane image before starting or updating services.

## Enable services

After both one-off tasks succeed:

```hcl
create_controlplane_service = true
```

Apply the change with the same variable-file selection used for the first
apply. The quickstart profile automatically loads `terraform.tfvars`; the
standard profile in this guide uses `standard.tfvars`. The composition starts
separate `helmr-controlplane` and `helmr-dispatcher` ECS services using
`controlplane_desired_count` and `dispatcher_desired_count`.

```sh
# infra/aws/quickstart
tofu apply

# infra/aws/standard
tofu apply -var-file=standard.tfvars

CONTROL_PLANE_URL="$(tofu output -raw controlplane_url)"
curl -fsS "$CONTROL_PLANE_URL/healthz"
curl -fsS "$CONTROL_PLANE_URL/readyz"
```

`/healthz` reports process liveness. `/readyz` is the traffic-readiness check after the database, Redis/Valkey, and schema are ready. Keep at least one dispatcher task when runs or schedules are used.

After readiness passes, sign in and finish first-organization setup using the setup token. Then add [workers](/docs/self-hosting/workers/) for task execution.
