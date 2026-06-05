---
title: Task secrets
description: Store remote secrets and declare task runtime placements.
section: Guides
sidebarLabel: Task secrets
order: 340
---

# Task secrets

Declare each runtime secret on the task:

```ts
export const useSecret = task({
  id: "use-secret",
  sandbox: sbx,
  secrets: [{ name: "API_TOKEN", env: "API_TOKEN" }],
  run: async () => {
    const token = process.env.API_TOKEN
    if (!token) throw new Error("API_TOKEN was not injected")
    return { ok: true }
  },
})
```

Store the secret value in Helmr:

```sh
printf '%s' "$API_TOKEN" | helmr secret set API_TOKEN
```

The task secret `name` is the Helmr secret name. If the task declares
`API_TOKEN`, store the value under that name:

```sh
helmr run use-secret
```

Run creation does not accept secret values or binding maps. Helmr resolves
declared secret names from the selected project environment when the run starts.

Secret placements can target:

- Environment variables: `{ name: "API_TOKEN", env: "API_TOKEN" }`
- Files: `{ name: "ssh-key", file: "secrets/token", mode: "0600" }`
- Directories: `{ name: "certs", dir: "secrets", mode: "0700" }`

Relative file and directory paths are materialized under the workspace. Absolute paths are materialized inside the image root and cannot target reserved runtime paths.
