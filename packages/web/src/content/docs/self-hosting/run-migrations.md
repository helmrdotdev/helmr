---
title: Run migrations
description: Run the database migration ECS task before starting control-plane services.
section: Self-hosting
sidebarLabel: Run migrations
order: 750
---

# Run migrations

Run database migrations after secrets are populated and before enabling the control and dispatcher services.

For the `standard` profile, the migration task runs in private subnets:

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

For the `quickstart` profile, use the profile output to decide whether the task needs a public IP:

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

Wait for the task to finish and inspect the logs if it exits non-zero. Do not start the control or dispatcher services until migrations have completed.
