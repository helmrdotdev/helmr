---
title: Projects
description: How projects and environments scope Helmr work.
section: Concepts
sidebarLabel: Projects
order: 110
---

# Projects

Projects group Helmr work inside an organization. A project has a slug, display name, default marker, environments, workspace repository access, task deployments, runs, secrets, and worker pools.

## Environments

Each project has environments. An environment is the scope where task deployments become active and runs execute. Secrets and worker pools are also environment-scoped.

Use separate environments when you need separate task versions, secret values, workers, or run history for the same product area.

## Workspace Repositories

A run can only use a GitHub workspace repository that is accessible to the Helmr GitHub App and enabled for the project. The run request includes `OWNER/REPO`, a ref or SHA, and optionally a subpath.

## Permissions

API keys are issued with explicit project and environment grants. Supported scopes include creating and reading runs, responding to waitpoints, using or writing secrets, and deploying tasks.
