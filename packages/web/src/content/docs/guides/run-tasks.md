---
title: Run tasks
description: Start a deployed task with payloads, secrets, and an attached workspace.
section: Guides
sidebarLabel: Run tasks
order: 320
---

# Run tasks

Remote session starts execute a deployed task in an attached workspace. If no
workspace is supplied, Helmr creates one from the task's deployed sandbox.

```sh
helmr session start hello
```

Pass payload as one JSON source:

```sh
helmr session start hello \
  --payload-json '{"name":"Ada"}'
```

or:

```sh
helmr session start hello \
  --payload-file payload.json
```

For simple string fields, use repeated `--payload` entries:

```sh
helmr session start cli-tooling \
  --payload pattern="export const"
```

Tasks that need secrets receive them through declarations in the task source.
Session starts provide payload and workspace attachment, not secret values:

```sh
helmr session start use-secret
```

Useful follow-up commands:

```sh
helmr run list
helmr run get RUN_ID
helmr run logs RUN_ID
helmr run events RUN_ID --follow
helmr run wait RUN_ID --timeout 10m
```
