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
  secrets: [
    { name: "API_TOKEN", env: "API_TOKEN" },
    { name: "config-json", file: "/run/secrets/config.json", mode: "0400" },
  ],
  run: async () => {
    return { hasToken: Boolean(process.env.API_TOKEN) }
  },
})
```

## Store Values

```sh
printf '%s' "$API_TOKEN" | helmr secret set API_TOKEN
```

The web UI lists secret names and timestamps, but it does not display saved values.

## Run With Secrets

```sh
helmr session start use-secret
```

Runs do not accept secret values or binding maps. Secret values are injected only at run time from the deployed task's declared secret names and should never be sent through payload.

## Names And Placement

Secret names must match `^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`. The `name` is the Helmr project-environment secret name. `env`, `file`, or `dir` declares where that value appears inside the runtime.
