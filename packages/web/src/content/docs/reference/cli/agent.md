---
title: helmr agent
description: Inspect Agent definitions and delete Agent identities.
sidebarLabel: agent
---

# `helmr agent`

```text
helmr agent list [--deployment DEPLOYMENT_ID | --all | --deleted] [--limit N] [--cursor CURSOR] [--json]
helmr agent get AGENT [--deployment DEPLOYMENT_ID] [--json]
```

`AGENT` is the `id` declared in `agent({ id: "support", ... })`. The response
calls this value `agent`. It is the only public Agent identifier.
Definition reads also include the default `template` and resolved `deployment_id`.
Use `helmr session create support` to create a Session.

Without `--deployment`, `--all` or `--deleted`, reads use the current Deployment. An explicit selector
inspects that exact Deployment, including historical content. An Agent absent
from the selected Deployment returns not found; the CLI does not choose an older
configuration. Configuration changes belong in code and a new Deployment.

| Flag | Description |
| --- | --- |
| `--deployment ID` | Inspect this Deployment instead of current. |
| `--all` | List retained identities, including Agents absent from current and deleted Agents. |
| `--deleted` | List only deleted identities. |
| `--limit N` | List page size, 1–100; default 50. |
| `--cursor CURSOR` | Continue the same Deployment snapshot or catalog view. |
| `--json` | Print the resource or page as JSON. |

Commands accept `-p, --project` and `-e, --env`. They are required with a saved
login and rejected with an API key. See [Credential selection](/docs/reference/cli/login#credential-selection).

## Retained Agent identities

`list --all` and `list --deleted` use the human management catalog and cannot be
combined with `--deployment` or each other. Use a saved login with `-p PROJECT
-e ENV` and unset `HELMR_API_KEY` for these reads. They work even when the
Environment has no current Deployment. JSON returns `agent`,
`current_deployment_id`, `defined_in_current`, `deleted_at` and `revision` per
identity, rather than a definition snapshot. `get` remains a definition read;
use the [status API](/docs/reference/api/agents) to inspect retained status directly.

## Delete

```text
helmr agent delete AGENT -p PROJECT -e ENV [--dry-run] [--if-match REVISION] [--json]
```

The command uses your saved human login, including when `HELMR_API_KEY` is set.
Delete requires `agents.delete` on the target Agent for preview and mutation.
It acts on the Environment's stable Agent identity, not a selected Deployment.

Preview first to see blockers, cleanup effects and warnings:

```sh
helmr agent delete support -p demo -e staging --dry-run --json
# Use the revision returned by the preview, for example 7:
helmr agent delete support -p demo -e staging --if-match 7
```

`--dry-run` changes nothing and does not require a revision. A preview with
`can_apply: false` is a successful inspection, not a successful deletion.
`--if-match` takes an unquoted revision number. If omitted on an apply, the CLI
reads the current revision before submitting the operation. A stale revision
fails; refresh the preview before trying again. Apply does not prompt for an
additional confirmation.

Delete requires the Agent to be absent from the current Deployment. Deleting
cancels its Sessions and the Sessions they started, including ones of other
Agents; `agents.delete` is the only permission it needs. The preview reports
their count as `sessions_cancelled`, and execution cleanup continues after
deletion. To restore a deleted Agent, promote a Deployment that declares it with
[`helmr deployment promote`](/docs/reference/cli/deployment); removed Role
selections, Slack Connections and old scheduled work stay removed. See
[Deleting and restoring an Agent](/docs/concepts/agents#deleting-and-restoring-an-agent).
