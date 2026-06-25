---
title: Projects
description: How projects and environments scope Helmr work.
section: Concepts
sidebarLabel: Projects
order: 110
---

# Projects

Projects group Helmr work inside an organization. A project has a slug, display name, environments, deployments, runs, and secrets.

## Environments

Each project has environments. An environment is the scope where deployments are labeled current and runs execute. Secrets are also environment-scoped. Worker instances provide organization-level compute capacity and are not created per project or environment.

Use separate environments when you need separate task versions, secret values, or run history for the same product area.

## Permissions

API keys are issued for one project environment. Supported permissions include creating and reading task sessions and runs, reading session streams, creating or completing tokens, using or writing secrets, and deploying tasks.
