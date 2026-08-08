---
title: helmr deploy
description: Build, create, wait for, and promote a Deployment.
sidebarLabel: deploy
---

# `helmr deploy`

```text
helmr deploy [path] [flags]
```

The command validates and packages the selected Helmr project, creates a
Deployment, streams build progress, and normally promotes a successful build.

| Flag | Meaning |
| --- | --- |
| `-p, --project`, `-e, --env` | Target scope. |
| `--timeout duration` | Maximum wait, default `20m`. |
| `--detach` | Queue the build and return without promotion. |
| `--skip-promotion` | Wait for the build but leave current unchanged. |
| `--no-image-cache` | Disable Platform layer cache import and export. |
| `--idempotency-key KEY` | Stable deployment-creation key. |
| `--json` | Emit progress as JSON lines. |

Human progress is written to stderr and the final version or Deployment ID to
stdout. `--detach` and `--skip-promotion` describe different stopping points.
