---
title: helmr status and doctor
description: Inspect CLI context and diagnose existing Helmr resources.
sidebarLabel: status / doctor
---

# `helmr status` and `helmr doctor`

```sh
helmr status --project PROJECT --env ENVIRONMENT
helmr status --offline --json
helmr doctor --project PROJECT --env ENVIRONMENT --json
```

`status` reports what exists. `doctor` checks the health of what exists. An empty
Environment, no current deployment, no Agents and no Slack connections are valid
states. Neither command creates resources, starts model calls, changes credentials
or repairs anything automatically.

Both accept `--api-url`, `--project`, `--env` and `--json`. `status` also accepts
`--offline`. Diagnostic requests share a 30-second deadline.

## Context and credentials

The report identifies the source of the server, Project and Environment:

- Server: `--api-url`, then `HELMR_API_URL`, then the API-key default origin or
  the saved login's origin, following normal CLI credential selection.
- Project: `--project`, then `HELMR_PROJECT`.
- Environment: `--env`, then `HELMR_ENV`.

The Project and Environment environment variables apply to these two diagnostic
commands only. Other Environment commands still require explicit scope flags
with a saved login. No Project or Environment context is saved.

With a saved login and no selected scope, the commands report identity and skip
Environment inspection. Selecting only one of Project or Environment is an error.
An API key is already Environment scoped and rejects explicit scope. Its Project
and Environment names remain unknown because there is no key-introspection API.

Credentials are described by kind, source and presence, never their value or a
prefix. Expiry is `null` because the current credential store and APIs do not
expose it. Offline mode makes no network requests and does not read the OS
credential store: saved credential presence is `null` (unknown), while an API key
in the environment can be reported as present. Server state and identity remain
`null` offline. Online commands use the existing OS credential store normally.

## Reported state and health

The commands read the current deployment, its Agents, up to ten recent Sessions,
and Environment attention counts when available. Agent listings follow all pages;
Session output includes a cursor if more Sessions exist. No model requirement is
inferred from an Agent name, harness or Secret name.

`doctor` reports existing Session holds and failed Turns as problems. With a saved
login, it uses the server's Environment-wide attention counts. With an API key,
it checks open Sessions among the latest ten Sessions. A failed Turn requires
attention only while it is the latest Turn in an open Session and terminated
within the past 24 hours; a newer Turn or Session closure clears that condition.
For API keys, this is a bounded view, not
an Environment-wide health guarantee. Questions awaiting a reply are reported as
state, not failures.

Owners and admins using a saved login can inspect Slack connections for retained
Agents, including Agents absent from the current deployment. Existing incomplete, disconnected or unauthorized connections need a
human to complete or repair them. A missing connection is not a failure. Where a
workspace connection exists, `doctor` reports the caller's Slack identity link.
A missing personal link is optional and does not fail the check; link it through
the Agent's first-mention prompt when you want to use that Agent.
Other roles, API keys and servers without Slack management available explicitly
skip Slack management inspection.

The existing server readiness probe checks database readiness. It does not prove
Worker capacity. Deployed Secret binding references, capacity and failed local build/upload attempt history are
explicitly skipped because the current read APIs do not expose them. A successful
report does not establish model-key validity or an end-to-end Slack reply.

## JSON and exit status

JSON output is one object, including on diagnostic failure. Its `schema` is
`helmr.status/v1` or `helmr.doctor/v1`; fields include `cli_version`, `offline`,
`api_url`, `project`, `environment`, `credential`, `identity`, `live` and `checks`.
Each check has an `id`, `status` (`pass`, `fail`, `error`, or `skip`) and `message`.
A remedy, when provided, includes a title and `requires_human`. Skipped checks
explain their limits. Unknown live state is `null`; successful empty lists are `[]`.

Both formats use the same exit status:

| Command | Exit 0 | Exit 1 |
| --- | --- | --- |
| `status` | Requested state was read, even if resources have problems. | State could not be read or configuration is invalid. |
| `doctor` | No executed check found a problem. Review skipped checks for unmeasured areas. | A check found a problem or could not be completed. |

The report goes to stdout. Command errors go to stderr. Diagnostics do not include
upstream error response bodies, which may contain sensitive data.
