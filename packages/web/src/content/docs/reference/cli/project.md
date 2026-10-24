---
title: Project and environment commands
description: Manage Projects and their Environments.
sidebarLabel: project and env
---

# Project and environment commands

```text
helmr project list [--json]
helmr project get PROJECT [--json]
helmr project create PROJECT [--name NAME] [--json]
helmr project update PROJECT --name NAME [--json]
helmr project settings get PROJECT [--json]
helmr project settings update PROJECT --history-retention DURATION|never [--json]
```

`update` requires at least one changed field.
`project list` traverses all available pages before writing output, so a failed
later page does not produce a partial text or JSON result.

`env` is a top-level command:

```text
helmr env list --project PROJECT [--json]
helmr env get ENVIRONMENT --project PROJECT [--json]
helmr env create ENVIRONMENT --project PROJECT [--name NAME] [--color '#RRGGBB'] [--json]
helmr env update ENVIRONMENT --project PROJECT [--name NAME] [--color '#RRGGBB'] [--json]
```

Project and Environment arguments use the same readable IDs shown in the Console
and returned as `project` and `environment` by the API. Create chooses the ID
explicitly; `--name` sets an optional display name. IDs are permanent. Update can
change the display name and Environment color. Environment color
defaults from its ID. Project IDs cannot use UUID syntax.

New Projects default to **30-day Session history retention**. Configure the
Project-wide policy in the Console's Project settings or with:

```sh
helmr project settings get PROJECT
helmr project settings update PROJECT --history-retention 30d
helmr project settings update PROJECT --history-retention never --json
```

`--history-retention` accepts a positive whole number followed by `s`, `m`, `h`, or
`d`, or `never` to disable automatic expiry. The minimum duration is one second.
Responses use the largest exact unit (`48h` becomes `2d`). JSON output is a settings
object such as `{"history_retention":"30d"}`. These commands use your login session;
reading and updating require Manage organization.

Changes apply to all remaining history, including existing Sessions, across every
Environment of the Project. Environment creation and editing have no retention
options. Shortening a period can permanently delete older content on the next
cleanup pass. CLI updates apply directly without an interactive confirmation.
Extending a period or choosing `never` cannot restore expired content.

The clock starts only after the Session ends, its process stops, and pending save,
continuation, answer and delivery obligations finish. Policy changes do not reset
it. Open Sessions retain their history. Very short periods can expire content
before delayed readers fetch it. Session identities and statuses, Computer disk
lifetime and administrative audit records are separate from history payload expiry.
