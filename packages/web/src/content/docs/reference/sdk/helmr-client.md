---
title: HelmrClient
description: Authenticated TypeScript client for the public v1 API.
---

# HelmrClient

```ts
const client = new HelmrClient({
  url: process.env.HELMR_API_URL,
  apiKey: process.env.HELMR_API_KEY!,
})
```

`apiKey` is required. `url` is optional and a custom `fetch` may be supplied.
All transport methods accept optional `{ signal }` as the final argument.

| Property | Operations |
| --- | --- |
| `tasks` | `retrieve`, `list`, `start` |
| `actors` | `retrieve`, `list`, `start` |
| `sessions` | `retrieve`, `list`, `ref` |
| `sandboxes` | `retrieve`, `list`, `createWorkspace` |
| `workspaces` | `retrieve`, `list`, `ref` |
| `runs` | `retrieve`, `list`, `cancel`, `wait`, `logs`, `events` |
| `deployments` | `list`, `current`, `retrieve` |
| `schedules` | `retrieve`, `list` |
| `secrets` | `create`, `retrieve`, `list`, `ref` |
| `tokens` | `create`, `retrieve`, `list`, `complete`, `cancel` |

List methods return `{ items, nextCursor? }`. Cursor values are opaque.
`runs.logs()` and `runs.events()` return finite pages; poll with the returned
cursor when following. `runs.retrieve()` and `runs.wait()` preserve a Task
output type when passed a typed `RunHandle`.
