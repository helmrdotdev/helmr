---
title: Overview
description: The self-hosted Helmr deployment path and the services you operate.
section: Self-hosting
sidebarLabel: Overview
order: 700
---

# Overview

Self-hosted Helmr runs in your AWS account with your own database, dispatch queue, object storage, secrets, OAuth login, control plane, dispatcher, and workers.

The deployment has these runtime components:

| Component | Responsibility |
| --- | --- |
| Control plane | Serves the web UI and API, stores run state in PostgreSQL, authenticates users, coordinates workers, and records logs/events. |
| Dispatcher | Reconciles runnable work into the Redis/Valkey dispatch path and sweeps expired executions. |
| Workers | Poll the control plane, materialize writable workspaces, build task images, run tasks in Firecracker guests, stream events, and create or restore checkpoints. |

AWS infrastructure provides the shared dependencies:

- Amazon RDS for PostgreSQL stores orgs, auth state, projects, runs, workers, waitpoints, checkpoints, and events.
- Cluster-mode disabled ElastiCache Valkey/Redis backs the dispatch queue used by
  `HELMR_REDIS_URL`.
- S3 stores source bundles, runtime artifacts, and encrypted checkpoint objects.
- AWS Secrets Manager stores database, auth, OAuth, worker, and encryption secrets.
- ECS Fargate runs the control, dispatcher, and migration tasks.
- EC2 Auto Scaling runs worker instances when task execution is enabled.

Use this sequence for a new environment:

1. Choose the AWS deployment profile.
2. Create the OAuth app and collect the non-secret client ID.
3. Configure non-secret values and create the base infrastructure.
4. Populate Secrets Manager.
5. Run database migrations.
6. Start the control and dispatcher services.
7. Add workers when you need actual run execution.
8. Verify a run from the CLI or UI.

The control plane can run without workers for login, deployments, API keys, and run inspection. Workers are required once runs need to execute code.
