---
title: helmr build
description: Build a verified deployment bundle locally.
sidebarLabel: build
---

# `helmr build`

```text
helmr build [path] [-o, --output DIR]
```

Builds a verified deployment bundle from the project at `path` (default `.`)
without contacting the control plane. Deploy the result with
[`helmr deploy --bundle DIR`](/docs/reference/cli/deploy). `helmr deploy`
without `--bundle` performs the same build itself.

| Flag | Description |
| --- | --- |
| `-o, --output DIR` | New bundle directory. Default `.helmr/deployment-bundle`. The directory must not already exist. |

On success the command prints the absolute output path.

## Requirements

- Node.js 22 or newer on `PATH`, to evaluate `helmr.config.ts`. Helmr does not
  download Node.js.
- The packages `helmr.config.ts` imports, installed on this machine (at least
  `@helmr/sdk`).
- Docker with Buildx, and a running selected Docker daemon.
- Every name in `build.secrets` set in the invoking environment.

## Steps

1. Capture the selected project source into a private directory.
2. Evaluate `helmr.config.ts` once on this machine.
3. Prepare the build environment from `build.builder`, when set.
4. Install project dependencies.
5. Analyze declarations, bundle the code, and build each Computer image.
6. Write the verified bundle to the output directory.

Source selection, the build environment, bundling and limits are described in
[Configuration](/docs/reference/config).

## Local bundle builder

`helmr build` and `helmr deploy` without `--bundle` use a dedicated Docker
Buildx builder with the `docker-container` driver. `helmr deploy --bundle` does
not use one.

| Behavior | Detail |
| --- | --- |
| Docker selection | An explicit `DOCKER_CONTEXT` takes precedence over `DOCKER_HOST`; otherwise Docker's current context applies. The selection, including its TLS and authentication settings, is fixed for the build. |
| `BUILDX_BUILDER` | Ignored. Helmr never changes the globally selected builder. |
| Builder name | `helmr-` followed by the first 24 lowercase hex characters of SHA-256 over `contextName + "\n" + endpointURI`. Shared by all projects on that context and endpoint. |
| Creation | Created when absent. An existing builder with that name must have exactly one `docker-container` node on the selected endpoint and be running after bootstrap. Helmr does not delete, append to or reconfigure an incompatible builder; it reports an error. |
| Retention | The builder container and its cache remain after success, failure or cancellation. |

To use custom BuildKit settings, create the builder with that name yourself
before building:

```sh
docker buildx create --name NAME --driver docker-container ENDPOINT
```

`ENDPOINT` is the selected context name, or the resolved endpoint URI for the
`default` context. Add `--buildkitd-config` or `--driver-opt` as needed; do not
pass `--use`. When no build is running, `docker buildx stop NAME` stops the
builder and `docker buildx rm NAME` removes it and its cache. The next build
recreates an absent builder.
