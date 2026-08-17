---
title: helmr deploy
description: Build, upload, finalize, and promote a Deployment bundle.
sidebarLabel: deploy
---

# `helmr deploy`

```text
helmr deploy [path] [flags]
```

The command builds the project in Helmr's digest-pinned Linux builder, uploads
only missing content-addressed objects, finalizes the verified bundle, and
promotes it unless requested otherwise. `--bundle` accepts an existing output
from `helmr build` and follows the same upload and verification path.

| Flag | Meaning |
| --- | --- |
| `-p, --project`, `-e, --env` | Target scope for a saved CLI login. |
| `--bundle PATH` | Deploy an existing verified bundle directory. |
| `--install-command COMMAND` | Override dependency preparation inside the isolated builder. |
| `--build-secret NAME` | Mount inherited `NAME` as `/run/secrets/NAME` during dependency installation; repeatable. |
| `--skip-promotion` | Finalize without making the Deployment current. |
| `--idempotency-key KEY` | Stable deployment-finalization key. |
| `--json` | Emit progress as JSON lines. |

Human progress is written to stderr and the final version to stdout. The
Control Plane never installs dependencies or rebuilds the uploaded artifacts.
