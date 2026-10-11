---
title: Project and environment commands
description: Manage Projects and their Environments.
sidebarLabel: project and env
---

# Project and environment commands

```text
helmr project list [--json]
helmr project get PROJECT [--json]
helmr project create NAME --history-retention-mode MODE [--history-retention-seconds SECONDS] [--slug SLUG] [--json]
helmr project update PROJECT [--name NAME] [--slug SLUG] [--json]
```

`update` requires at least one changed field.
`project list` traverses all available pages before writing output, so a failed
later page does not produce a partial text or JSON result.

`env` is a top-level command:

```text
helmr env list --project PROJECT [--json]
helmr env get ENVIRONMENT --project PROJECT [--json]
helmr env create NAME --project PROJECT --history-retention-mode MODE [--history-retention-seconds SECONDS] [--slug SLUG] [--color '#RRGGBB'] [--json]
helmr env update ENVIRONMENT --project PROJECT [--name NAME] [--slug SLUG] [--color '#RRGGBB'] [--json]
```

Project and Environment arguments accept a slug or ID. Generated slugs are
used when `--slug` is omitted; Environment color also defaults from the slug.
Project slugs cannot use UUID syntax because UUID-shaped references are project
IDs.

History retention must be selected explicitly when creating a Project or Environment. Use `--history-retention-mode duration --history-retention-seconds SECONDS` with a positive whole number of seconds, or `--history-retention-mode until_environment_deletion` without seconds. Project creation applies the selection to both initial Environments. `helmr env update` accepts the same flags to change the policy for future Sessions; existing Sessions keep their original policy.

A duration starts only after the Session has ended, its process has stopped, and required save, continuation, answer and delivery obligations have been released. Open Sessions retain their history. Computer disk lifetime is separate from Session history retention.
