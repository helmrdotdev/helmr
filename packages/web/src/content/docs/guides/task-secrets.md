---
title: Task secrets
description: Store remote secrets and declare task runtime placements.
section: Guides
sidebarLabel: Task secrets
order: 340
---

# Task secrets

Store the secret value in Helmr:

```sh
printf '%s' "$API_TOKEN" | helmr secret create API_TOKEN
```

List or inspect stored secret metadata without revealing values:

```sh
helmr secret list
helmr secret get API_TOKEN
```

Place it when creating the Workspace:

```sh
WORKSPACE_ID="$(helmr workspace create app-workspace \
  --key secret-demo \
  --secret-env API_TOKEN=API_TOKEN \
  --idempotency-key secret-demo-workspace)"
```

Revoke a Secret when Tasks should no longer be able to resolve it. Revocation
blocks every retained version and future rotation without deleting the stable
Secret identity or its immutable encrypted history:

```sh
helmr secret revoke API_TOKEN --yes
```

The task secret `name` is the Helmr secret name. If the task declares
`API_TOKEN`, store the value under that name:

```sh
helmr run start use-secret --workspace "${WORKSPACE_ID}"
```

Run creation does not accept secret values or binding maps.

Secret placements can target:

- Environment variables: `{ name: "API_TOKEN", env: "API_TOKEN" }`
- Files: `{ name: "ssh-key", file: "/run/secrets/token" }`
- Directories: `{ name: "certs", dir: "secrets", mode: "0700" }`

Relative file and directory paths are materialized under the workspace. Absolute paths are materialized inside the image root and cannot target reserved runtime paths.
