---
title: TypeScript SDK
description: Public exports and the two execution contexts of @helmr/sdk.
sidebarLabel: Overview
---

# TypeScript SDK

Import public APIs from `@helmr/sdk`. The package has two related surfaces:

- declaration and guest-runtime APIs used by deployed code: `task`, `actor`,
  `sandbox`, `schedules`, `tokens`, `timers`, `logger`, and `metadata`;
- `HelmrClient`, an authenticated HTTP client for code running outside Helmr.

`index.ts` is the public export authority. Modules not re-exported there are
internal even if their source is visible.

```ts
import {
  defineConfig,
  HelmrClient,
  sandbox,
  task,
} from "@helmr/sdk"
```

Definition IDs match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Public values passed
as payloads, outputs, metadata, token results, or log attributes must be JSON
values. Durable operations are asynchronous and should be awaited.
