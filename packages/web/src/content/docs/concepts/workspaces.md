---
title: Workspaces
description: GitHub checkout inputs and runtime filesystem behavior.
section: Concepts
sidebarLabel: Workspaces
order: 150
---

# Workspaces

A workspace is the GitHub source checked out for a run. It is identified by repository, ref or SHA, and optional subpath. When a subpath is set, only that directory is materialized in the sandbox workspace.

```sh
helmr run review-pr \
  --repo OWNER/REPO \
  --ref main \
  --subpath tools/review \
  --payload-json '{"prNumber":123}'
```

In SDK-triggered runs, use `workspace.github("OWNER/REPO", { ref, subpath })`.

## Repository Access

The workspace repository must be accessible to the configured Helmr GitHub App and enabled for the project. The control plane resolves the requested ref to a concrete GitHub source before a worker receives the run.

## Runtime Directory

Tasks start at the workspace mount path. For whole-repository runs this is the repository root; for subpath runs this is the selected subdirectory. Use relative paths for workspace files. Absolute paths follow normal Linux filesystem behavior inside the guest.

## Subpaths

Use `--subpath` when the task project or target files live under a repository subdirectory. The subpath is the materialization boundary for the run: files above it are not present in the sandbox workspace.
