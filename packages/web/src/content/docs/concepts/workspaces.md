---
title: Workspaces
description: Runtime workspace filesystem behavior.
section: Concepts
sidebarLabel: Workspaces
order: 150
---

# Workspaces

A workspace is the writable directory mounted for a run. Helmr creates it empty by default and starts the task at the workspace project path.

```sh
helmr run review-pr \
  --payload-json '{"owner":"OWNER","repo":"REPO","prNumber":123}' \
  --secret GITHUB_TOKEN=vault:github-token
```

If a task needs repository files, declare the required token as a secret and clone or fetch the repository inside the task.

## Runtime Directory

Tasks start at the workspace mount path. Use relative paths for workspace files. Absolute paths follow normal Linux filesystem behavior inside the guest.

## Repository Access

Repository access is a task-level integration. Pass repository identifiers in payload and credentials through declared secrets; the control plane does not resolve or materialize a repository for a run.
