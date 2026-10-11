---
title: helmr agent
description: Inspect deployed Agent definitions and start Sessions.
sidebarLabel: agent
---

# `helmr agent`

```text
helmr agent definition list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr agent definition get DEFINITION_ID [--deployment DEPLOYMENT_ID] [--json]
helmr agent start AGENT (--text TEXT | --input-json JSON | --input-file FILE) [--computer COMPUTER_ID]
    [--session-key KEY] [--idempotency-key KEY] [--slack-channel CHANNEL_ID] [--json]
```

Every command also accepts `-p, --project` and `-e, --env`. They are required
with a saved login and rejected with an API key. See
[Credential selection](/docs/reference/cli/login#credential-selection).

## `definition list` and `definition get`

| Flag | Description |
| --- | --- |
| `--deployment DEPLOYMENT_ID` | Read this Deployment. Defaults to the current Deployment. |
| `--limit N` | Page size, 1–100. Default 50. |
| `--cursor CURSOR` | Continuation cursor from the previous page. The cursor keeps the Deployment it started with. |
| `--json` | Print one JSON page or definition. |

Each definition has its definition `id` and an `agent_id`, the Agent's stable
UUID across Deployments. `helmr session list --agent` filters by that UUID.

## `start`

Starts a Session and its first Turn for the Agent with definition ID `AGENT`
in the current Deployment.

| Flag | Description |
| --- | --- |
| `--text TEXT` | Input as one text part, with whitespace preserved. |
| `--input-json JSON` | Input as a JSON array of text parts, for example `'[{"type":"text","text":"Review APP-42"}]'`. |
| `--input-file FILE` | Read the input JSON array from a file. |
| `--computer COMPUTER_ID` | Run on an existing Computer. Omit to create one from the Agent's Computer definition. |
| `--session-key KEY` | Reuse the Session with this key, or create it. |
| `--idempotency-key KEY` | Retry identity. Reuse the key with identical arguments to retry an uncertain request. |
| `--slack-channel CHANNEL_ID` | Open the Session in this Slack channel, given as a raw channel ID such as `C0123456789`. Supply at most once. See [Slack](/docs/concepts/slack). |
| `--json` | Print the full receipt as JSON. |

Exactly one of `--text`, `--input-json` or `--input-file` is required. Use
`--input-json '[]'` for empty input.

Text output prints `session_id` and `turn_id`. JSON output is the receipt:

```json
{
  "session_id": "…",
  "turn_id": "…",
  "sequence": 1,
  "created": true,
  "close_after_idle_ms": 604800000
}
```

`close_after_idle_ms` is `null` when idle closure is disabled. The receipt
confirms admission, not completion. Continue with
[`helmr session`](/docs/reference/cli/session).

```sh
helmr agent start hello --text "Say hello" --project my-project --env dev
```
