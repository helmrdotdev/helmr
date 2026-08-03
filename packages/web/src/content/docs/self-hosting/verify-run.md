---
title: Verify a run
description: Confirm that login, workers, and run execution work together.
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

Deploy the task project from a local checkout:

```sh
helmr deploy . --project PROJECT --env ENVIRONMENT
```

Create a Workspace, start a small Task Run, and watch it reach a terminal state:

```sh
WORKSPACE_ID="$(helmr workspace create WORKSPACE_DECLARED_ID \
  --key verify \
  --idempotency-key verify-workspace)"
helmr task start TASK_ID --workspace "${WORKSPACE_ID}" \
  --idempotency-key verify-run --wait --follow
helmr run list
helmr run logs RUN_ID
```

A complete smoke test proves that:

- GitHub OAuth is configured.
- The control plane can read project metadata.
- A worker is active and can lease work.
- The worker can reach S3, ECR, AWS APIs, the control plane, and any external services used by the task.
- Firecracker and the Worker-owned routed-TAP datapath are available, and the certified guest rootfs contains the pinned BuildKit daemon used by image-build guests.

If a run stays queued, check worker capacity first. If a task cannot reach an external repository or API, check the task secret and worker egress path.
