# Slack Approval

This example shows a Slack approval backed by a Helmr external completion token.
The trusted Slack bridge creates one token, posts Slack buttons, starts the
Helmr task with that token id, and completes the token when a signed Slack
action arrives.

## Deploy and Start the Bridge

```sh
helmr deploy examples/slack-approval

export HELMR_API_URL="https://dev.helmr.dev"
export HELMR_API_KEY="..."
export HELMR_TASK_ID="slack-approval"
export SLACK_BOT_TOKEN="xoxb-..."
export SLACK_SIGNING_SECRET="..."
export SLACK_CHANNEL_ID="C0123456789"
export APPROVAL_RELEASE="helmr-web-2026-06-14"
export APPROVAL_TITLE="Approve helmr-web-2026-06-14?"
export APPROVAL_SUMMARY="Promote the validated staging build to production."
export APPROVAL_RISK="Touches run input delivery."
export APPROVAL_STAGING_URL="https://staging.example.com"
export APPROVAL_PRODUCTION_URL="https://example.com"

bun run --cwd examples/slack-approval bridge
```

Configure Slack app interactivity to call the bridge:

```text
https://your-bridge.example.com/slack/actions
```

The Slack app needs the `chat:write` bot scope and the bot user must be able to
post in the selected channel. The bridge verifies Slack signatures before
accepting button clicks.

## Flow

1. The trusted bridge creates a Helmr token with `timeout: "7d"`.
2. The bridge posts Slack buttons whose value contains the token id.
3. The bridge starts the `slack-approval` task with the token id in its payload.
4. The task waits on `tokens.wait(tokenId, { timeout: "7d" })`.
5. A signed Slack button click completes the token with the bridge API key.
6. Repeated clicks are deterministic: the same completion remains accepted, and
   conflicting completions return a stable "already recorded" response.

The bridge posts Slack before starting the task. If Slack posting fails, it
cancels the token and does not create a parked run that cannot be approved from
Slack. By default the bridge derives Helmr idempotency keys from the task,
channel, and release; set `HELMR_TOKEN_IDEMPOTENCY_KEY` and
`HELMR_START_IDEMPOTENCY_KEY` when you need a different retry boundary.

The long approval timeout belongs to the token wait, not to active task compute.
The task's `maxDuration` remains a short active budget because parked token
waits release compute.

Use session streams for session-scoped timelines such as chat steering,
multiple messages, or progress records. Use tokens for one-shot external
completion that does not need to be bound to a session stream, such as Slack,
email, or browser-link approvals. Use timers for wall-clock waits without
active compute budget.
