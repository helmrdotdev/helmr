---
title: Vault secrets
description: Store remote secrets and bind them to declared task placements.
section: Guides
sidebarLabel: Vault secrets
order: 340
---

# Vault secrets

Declare each runtime secret on the task:

```ts
export const useSecret = task({
  id: "use-secret",
  sandbox: sbx,
  secrets: {
    API_TOKEN: { env: "API_TOKEN" },
  },
  run: async () => {
    const token = process.env.API_TOKEN
    if (!token) throw new Error("API_TOKEN was not injected")
    return { ok: true }
  },
})
```

Store the secret value in Helmr:

```sh
printf '%s' "$API_TOKEN" | helmr secret set api-token
```

Bind the declared task name to the stored vault name when creating a run:

```sh
helmr run use-secret \
  --secret API_TOKEN=vault:api-token
```

Remote runs accept vault bindings only: `NAME=vault:SECRET_NAME`. `env:` and `file:` are not valid remote secret sources.

Secret placements can target:

- Environment variables: `{ env: "API_TOKEN" }`
- Files: `{ file: "secrets/token", mode: "0600" }`
- Directories: `{ dir: "secrets", mode: "0700" }`

Relative file and directory paths are materialized under the workspace. Absolute paths are materialized inside the image root and cannot target reserved runtime paths.
