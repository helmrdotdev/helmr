---
title: Project and environment commands
description: Manage Projects and their Environments.
sidebarLabel: project and env
---

# Project and environment commands

```text
helmr project list [--json]
helmr project get PROJECT [--json]
helmr project create PROJECT [--history-retention-mode MODE] [--history-retention-seconds SECONDS] [--name NAME] [--json]
helmr project update PROJECT --name NAME [--json]
```

`update` requires at least one changed field.
`project list` traverses all available pages before writing output, so a failed
later page does not produce a partial text or JSON result.

`env` is a top-level command:

```text
helmr env list --project PROJECT [--json]
helmr env get ENVIRONMENT --project PROJECT [--json]
helmr env create ENVIRONMENT --project PROJECT [--history-retention-mode MODE] [--history-retention-seconds SECONDS] [--name NAME] [--color '#RRGGBB'] [--json]
helmr env update ENVIRONMENT --project PROJECT [--name NAME] [--color '#RRGGBB'] [--history-retention-mode MODE] [--history-retention-seconds SECONDS] [--json]
```

Project and Environment arguments use the same readable IDs shown in the Console
and returned as `project` and `environment` by the API. Create chooses the ID
explicitly; `--name` sets an optional display name. IDs are permanent. Update can
change the display name, Environment color and history policy. Environment color
defaults from its ID. Project IDs cannot use UUID syntax.

New Projects and Environments keep Session history without a time limit by default, until the Environment is deleted. Configure retention in the Console’s Environment settings or with `helmr env update`. Creation commands also accept an explicit policy. Use `--history-retention-mode duration --history-retention-seconds SECONDS` with a positive whole number of seconds, or `--history-retention-mode until_environment_deletion` without seconds. Project creation applies the selection to both initial Environments. `helmr env update` accepts the same flags to change the policy for future Sessions; existing Sessions keep their original policy.

A duration starts only after the Session has ended, its process has stopped, and required save, continuation, answer and delivery obligations have been released. Open Sessions retain their history. Computer disk lifetime is separate from Session history retention.
