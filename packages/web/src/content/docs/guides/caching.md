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

const installNode24 = [
  "apt-get update",
  "apt-get install -y --no-install-recommends ca-certificates curl gnupg",
  "install -d -m 0755 /etc/apt/keyrings",
  "curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg",
  "echo 'deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main' > /etc/apt/sources.list.d/nodesource.list",
  "apt-get update",
  "apt-get install -y --no-install-recommends nodejs",
  "rm -rf /var/lib/apt/lists/*",
].join(" && ")

const deps = image("dependency-cache-deps")
  .from("oven/bun:1.3.10-debian")
  .workdir("/opt/app")
  .run(["sh", "-ceu", installNode24])
  .copy("/opt/app/package.json", source.file("app/package.json"))
  .copy("/opt/app/bun.lock", source.file("app/bun.lock"))
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun", cache: cache("bun-global") }],
  })
```

Cache ids are named in task source with `cache("...")`. Use stable ids for dependency caches you want to reuse across builds of the same task project.

Keep dependency inputs explicit. Copy lockfiles and package manifests before the install step so image rebuilds are tied to dependency changes.

A single image `run` step cannot combine persistent cache mounts and build secret mounts. Split those operations into separate `run` steps when you need both.

Deploy archives exclude `node_modules` by default. Remote deployment builds install project dependencies in a product-managed build environment, but task execution does not use deployment build dependencies. Install runtime dependencies inside the image build.
