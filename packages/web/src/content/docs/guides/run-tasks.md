---
title: Run tasks
description: Start a deployed task with payloads and secrets.
section: Guides
sidebarLabel: Run tasks
order: 320
---

# Run tasks

Remote runs execute a deployed task with an empty writable workspace.

```sh
helmr run hello
```

Pass payload as one JSON source:

```sh
helmr run hello \
  --payload-json '{"name":"Ada"}'
```

or:

```sh
helmr run hello \
  --payload-file payload.json
```

For simple string fields, use repeated `--payload` entries:

```sh
helmr run cli-tooling \
  --payload pattern="export const"
```

Tasks that need secrets receive them through declarations in the task source. Runs only provide payload:

```sh
helmr run use-secret
```

Useful follow-up commands:

```sh
helmr ps
helmr show RUN_ID
helmr logs RUN_ID
helmr events RUN_ID --follow
helmr wait RUN_ID --timeout 10m
```
