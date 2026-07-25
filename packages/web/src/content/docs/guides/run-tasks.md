---
title: Run tasks
description: Create a Workspace and start a deployed Task Run.
section: Guides
sidebarLabel: Run tasks
order: 320
---

# Run tasks

Create a Workspace from a deployed declaration, then start the Task in that
Workspace:

```sh
WORKSPACE_ID="$(helmr workspace create app-workspace \
  --key demo \
  --idempotency-key workspace:demo)"

helmr task start hello \
  --workspace "${WORKSPACE_ID}" \
  --idempotency-key hello:demo \
  --payload-json '{"name":"Ada"}'
```

Payload can come from `--payload-json`, `--payload-file`, or repeated
`--payload KEY=VALUE` flags. Secret values are not payload fields; place
declared Secrets when creating the Workspace.

Use `--wait` for the terminal snapshot or `--follow` to print finite log pages
until the Run becomes terminal:

```sh
helmr task start hello \
  --workspace "${WORKSPACE_ID}" \
  --idempotency-key hello:demo:2 \
  --payload-file payload.json \
  --wait
```

Useful inspection commands:

```sh
helmr run list
helmr run get RUN_ID
helmr run logs RUN_ID
helmr run events RUN_ID
helmr run wait RUN_ID --timeout 10m
```
