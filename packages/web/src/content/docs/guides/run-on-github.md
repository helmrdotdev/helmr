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

For simple string fields, use `-p`:

```sh
helmr run cli-tooling \
  -p pattern="export const"
```

Bind declared secrets with vault references. Tasks that need repository access should receive repository identifiers in payload and credentials through secrets:

```sh
helmr run github-pr-review \
  --payload-json '{"owner":"OWNER","repo":"REPO","prNumber":42}' \
  --secret GITHUB_TOKEN=vault:github-token
```

Useful follow-up commands:

```sh
helmr ps
helmr show RUN_ID
helmr logs RUN_ID
helmr events RUN_ID
```
