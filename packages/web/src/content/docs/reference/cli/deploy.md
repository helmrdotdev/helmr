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
| `--skip-promotion` | Finalize without making the Deployment current. |
| `--idempotency-key KEY` | Stable deployment-finalization key. |
| `--json` | Emit progress as JSON lines. |
| `--upload-timeout DURATION` | Deadline for each object, including capacity retries; default `3h`, configurable with `HELMR_UPLOAD_TIMEOUT`. |
| `--upload-idle-timeout DURATION` | Time without upload progress; default `2m`, configurable with `HELMR_UPLOAD_IDLE_TIMEOUT`. |

The install command, build secrets and build environment are
[build settings](/docs/reference/config#build-environment) in `helmr.config.ts`, which the command evaluates
once on this machine (Node.js 22 or newer required).

Human progress is written to stderr and the final version to stdout. The
Control Plane never installs dependencies or rebuilds the uploaded artifacts.

Uploads use the configured Helmr origin with current authorization. When upload
capacity is busy, the CLI honors `Retry-After` within the per-object deadline.
Explicit flags override environment settings; both durations must be positive
and the idle timeout must not exceed the total timeout. Server and ingress
limits apply independently. Reserve time for capacity retries when matching
client settings to the server's admitted-transfer duration.
