---
title: Deployments and environments
description: How projects, environments, builds, and promotion establish runtime authority.
sidebarLabel: Deployments and environments
---

# Deployments and environments

A Project groups a product or service inside an organization. Environments are
independent scopes within a Project: each has its own current Deployment,
Secrets, Workspaces, Runs, Sessions, Tokens, and Schedules.

Use separate Environments when the same source needs different promoted
versions, credentials, execution history, or access boundaries. Environment
API keys are issued into one such scope. Saved user sessions instead select a
Project and Environment on each CLI operation.

A Deployment is one immutable, content-addressed bundle produced by the Helmr
CLI on a developer or CI machine. The official builder installs the project's
dependencies, evaluates config, compiles declarations, builds Sandbox images,
and indexes Tasks, Actors, Sandboxes, and Schedules. The Control Plane verifies
and registers the completed closure without rebuilding it.

Promotion makes one completed Deployment current in an Environment. New
definition lookups and starts use that current Deployment unless the read API
explicitly selects another Deployment. Existing Runs remain pinned to the
Deployment recorded when they were created.

Promotion also reconciles source-declared Schedules. It does not mutate the
immutable Deployment or rewrite existing Workspaces. Deploying with
`--skip-promotion` is useful for finalizing and inspecting a candidate before it
becomes current.

The TypeScript SDK's developer routes use `/v1` with an environment API key.
Authenticated management routes use explicit project and environment prefixes.
Those are two authentication shapes over the same scoped resources, not two
different product models.
