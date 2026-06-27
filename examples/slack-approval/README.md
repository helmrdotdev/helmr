# Slack Approval

This example shows a Slack approval backed by Helmr session input. The trusted
bridge verifies Slack signatures, starts one Helmr session with a Slack-shaped
`externalId`, posts Slack buttons, and records the signed Slack action as durable
session input with `HELMR_API_KEY`.

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
# Optional. Defaults to slack:${SLACK_CHANNEL_ID}:${APPROVAL_RELEASE}.
export HELMR_SESSION_EXTERNAL_ID="slack:C0123456789:helmr-web-2026-06-14"

bun run --cwd examples/slack-approval bridge
```

Configure Slack app interactivity to call the bridge:

```text
https://your-bridge.example.com/slack/actions
```

The Slack app needs the `chat:write` bot scope and the bot user must be able to
post in the selected channel. The bridge verifies Slack signatures before
accepting button clicks.

The Slack button value carries only the session external id and the approval
decision. Slack never receives a Helmr bearer capability; the bridge owns
`HELMR_API_KEY` and appends the input after signature verification.

## Flow

1. The bridge starts a Helmr session with `externalId:
   slack:${SLACK_CHANNEL_ID}:${APPROVAL_RELEASE}`.
2. The task waits on `streams.input("approval").wait({ correlationId,
   timeout: "7d" })`.
3. The bridge posts Slack buttons whose value carries the external id and
   approval decision.
4. A signed Slack button click appends `{ approved, actor, channelId }` to the
   session input stream with
   `client.sessions.open({ externalId }).input("approval").send(...)`.
5. Repeated clicks receive a stable "already recorded" response after the
   idempotency key has accepted the first decision.

If setup fails after the session starts, the bridge cancels the parked session
so the approval does not sit waiting forever. By default the bridge derives
Helmr idempotency keys from the task, channel, and release; set
`HELMR_START_IDEMPOTENCY_KEY` when you need a different retry boundary.

The long approval timeout belongs to the stream wait, not to active task
compute. The task's `maxDuration` remains a short active budget because parked
stream waits release compute.

Use session input when the external system is part of the session timeline, as
Slack buttons, Slack messages, chat steering, browser controls, or progress
events are. Create public stream tokens when a browser, mobile client, email
link, or third-party callback must call Helmr without `HELMR_API_KEY`. Use
tokens for one-shot completion that is independent of a session stream, such as
a standalone provider callback. Use timers for wall-clock waits without active
compute budget.
