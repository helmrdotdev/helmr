---
title: Secrets
description: Declaring, storing, and binding run-time secret values.
section: Concepts
sidebarLabel: Secrets
order: 180
---

# Secrets

Secrets are encrypted values stored by name and scoped to a project
environment. Workspace creation declares where each Secret appears in the
guest:

```sh
helmr workspace create app-workspace \
  --key app \
  --secret-env API_TOKEN=API_TOKEN \
  --secret-file config-json=/run/secrets/config.json \
  --idempotency-key app-workspace
```

## Store Values

```sh
printf '%s' "$API_TOKEN" | helmr secret create API_TOKEN
```

The web UI lists secret names and timestamps, but it does not display saved values.

## Run With Secrets

```sh
helmr run start use-secret --workspace WORKSPACE_ID
```

Runs do not accept secret values or binding maps. They use the placements fixed
when their Workspace was created. Secret values should never be sent through
payload.

## Names And Placement

Secret names must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. The `name` is the
Helmr project-environment Secret name. A Workspace placement selects one
environment variable or one absolute file path.
