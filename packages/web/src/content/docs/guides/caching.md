---
title: Caching
description: Reuse dependency downloads during image builds.
section: Guides
sidebarLabel: Caching
order: 360
---

# Caching

Use cache mounts on image build steps for package manager caches.

```ts
import { cache, image, source } from "@helmr/sdk"

const deps = image("dependency-cache-deps")
  .from("node:24-bookworm-slim")
  .workdir("/opt/app")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/app/package.json", source.file("app/package.json"))
  .copy("/opt/app/bun.lock", source.file("app/bun.lock"))
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun", cache: cache("bun-global") }],
  })
```

Cache ids are named in task source with `cache("...")`. Use stable ids for dependency caches you want to reuse across builds of the same task project.

Keep dependency inputs explicit. Copy lockfiles and package manifests before the install step so image rebuilds are tied to dependency changes.

A single image `run` step cannot combine persistent cache mounts and build secret mounts. Split those operations into separate `run` steps when you need both.

Exclude root `node_modules` explicitly in `.helmrignore`; retaining it makes
source admission fail. The remote Program build installs project dependencies.
Workspace-image dependencies remain separate and must be installed by the image
plan when Workspace tools need them.
