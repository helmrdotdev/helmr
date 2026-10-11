---
title: helmr agent
description: Inspect deployed Agent definitions and start Sessions.
sidebarLabel: agent
---

# `helmr agent`

```text
helmr agent list [--deployment DEPLOYMENT_ID] [--limit N] [--cursor CURSOR] [--json]
helmr agent get DEFINITION_ID [--deployment DEPLOYMENT_ID] [--json]
helmr agent start DEFINITION_ID (--input-json JSON | --input-file FILE) [flags]
```

`list` and `get` inspect the current Deployment unless `--deployment` pins an
explicit Deployment. A list cursor retains the original Deployment snapshot.
Lists default to 50 definitions and accept up to 100. Definition IDs are authored
names; Session discovery's `--agent` filter uses the Agent UUID instead.

`start` admits a Session and its first Turn. Its receipt includes `session_id`,
`turn_id`, admission `sequence` and whether the Session was `created`. Use
[`helmr session`](/docs/reference/cli/session) to inspect and continue that work. An admission
receipt does not mean execution or saving has completed.

Options include `--computer COMPUTER_ID` for explicit placement,
`--session-key KEY` for explicit conversation identity, `--idempotency-key KEY`
for request retries, and `--slack-channel CHANNEL_CONFIGURATION_ID` for a selected
approved Slack route. Omitting `--computer` prepares a Computer from the Agent's
definition. Retry an uncertain request with the same key and arguments.

All commands support `--json`. Saved-login requests require `--project` and
`--env`; an API key already supplies Environment scope.
