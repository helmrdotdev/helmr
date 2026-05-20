---
title: Verify a run
description: Confirm that login, GitHub access, workers, and run execution work together.
section: Self-hosting
sidebarLabel: Verify a run
order: 780
---

# Verify a run

After the control and dispatcher services are ready and at least one worker is active, verify the environment with a small run. A default `quickstart` stack must enable NAT and worker capacity before this step.

Log in to the control URL:

```sh
helmr login "$CONTROL_URL"
```

Install the GitHub App on a repository that contains a Helmr project, then deploy the task project from a local checkout:

```sh
helmr deploy .
```

Start a small task run against the GitHub repository and watch it reach a terminal state:

```sh
RUN_ID=$(helmr run TASK_ID --repo OWNER/REPO --ref main)
helmr ps
helmr logs "$RUN_ID"
```

A complete smoke test proves that:

- GitHub OAuth and App installation are configured.
- The control plane can read project metadata.
- A worker is active and can lease work.
- The worker can reach GitHub, S3, ECR, AWS APIs, and the control plane.
- Firecracker, BuildKit, CNI, and guest artifacts are present on the worker.

If a run stays queued, check worker capacity first. If checkout fails, check GitHub App installation and repository access.
