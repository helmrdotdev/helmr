---
title: Run on GitHub
description: Start a deployment task against a GitHub checkout.
section: Guides
sidebarLabel: Run on GitHub
order: 320
---

# Run on GitHub

Remote runs execute a deployment task against a GitHub workspace.

```sh
helmr run hello \
  --repo OWNER/REPO \
  --ref main
```

The repository must be accessible to the Helmr GitHub App configured for the control plane.

Use `--subpath` to materialize only part of the repository as the sandbox workspace:

```sh
helmr run hello \
  --repo OWNER/REPO \
  --ref main \
  --subpath path/to/project
```

Pass payload as one JSON source:

```sh
helmr run hello --repo OWNER/REPO --ref main \
  --payload-json '{"name":"Ada"}'
```

or:

```sh
helmr run hello --repo OWNER/REPO --ref main \
  --payload-file payload.json
```

For simple string fields, use `-p`:

```sh
helmr run cli-tooling --repo OWNER/REPO --ref main \
  -p pattern="export const"
```

Bind declared secrets with vault references:

```sh
helmr run github-pr-review \
  --repo OWNER/REPO \
  --ref main \
  --payload-json '{"prNumber":42}' \
  --secret GITHUB_TOKEN=vault:github-token
```

Useful follow-up commands:

```sh
helmr ps
helmr show RUN_ID
helmr logs RUN_ID
helmr events RUN_ID
```
