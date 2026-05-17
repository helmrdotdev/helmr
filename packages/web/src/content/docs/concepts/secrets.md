---
title: Secrets
description: Declaring, storing, and binding run-time secret values.
section: Concepts
sidebarLabel: Secrets
order: 180
---

# Secrets

Secrets are encrypted values stored by name and scoped to a project environment. Task code must declare where each secret should appear inside the guest.

```ts
export const useSecret = task({
  id: "use-secret",
  sandbox: sb,
  secrets: {
    API_TOKEN: { env: "API_TOKEN" },
    CONFIG_JSON: { file: "/run/secrets/config.json", mode: "0400" },
  },
  run: async () => {
    return { hasToken: Boolean(process.env.API_TOKEN) }
  },
})
```

## Store Values

```sh
printf '%s' "$API_TOKEN" | helmr secret set api-token
```

The web UI lists secret names and timestamps, but it does not display saved values.

## Bind Values To Runs

```sh
helmr run use-secret \
  --repo OWNER/REPO \
  --ref main \
  --secret API_TOKEN=vault:api-token
```

Remote runs bind declared task secrets to vault references. Secret values are injected only at run time and should never be sent through payload.

## Names And Placement

SDK secret names must match `^[A-Za-z_][A-Za-z0-9_]*$` and be at most 128 characters. Placements can be environment variables, files, or directories.
