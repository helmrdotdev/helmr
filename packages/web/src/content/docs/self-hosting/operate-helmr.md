---
title: Operate Helmr
description: Day-two operations for control, workers, secrets, checkpoints, and upgrades.
section: Self-hosting
sidebarLabel: Operate Helmr
order: 790
---

# Operate Helmr

Use these checks when operating a self-hosted environment.

| Area | Practice |
| --- | --- |
| Control rollout | Run migrations before starting a new control image. Use `/readyz` as the readiness probe. |
| Workers | Drain workers before host replacement or AMI rollout. Scale capacity from Auto Scaling settings, not by manually editing instances. |
| Database | Keep RDS backups and deletion protection enabled for production. Restore into a separate environment before destructive testing. |
| Secrets | Rotate GitHub and auth secrets from Secrets Manager. Keep secret values out of Terraform variables and logs. |
| Checkpoints | Keep the same checkpoint encryption key available to all workers that may restore a paused run. |
| Networking | Private workers need outbound access to GitHub, S3, ECR, AWS APIs, and the control URL. |

Worker status is command based:

```sh
helmr-worker status
```

The command exits non-zero unless the worker can authenticate to the control plane and is active.

Checkpoint restore verifies runtime compatibility before resuming a run. Worker backend, architecture, ABI, kernel digest, rootfs digest, runtime config digest, vCPU count, memory, and CNI profile must match the checkpoint metadata.

Checkpoint objects are encrypted by the worker before upload to object storage.
