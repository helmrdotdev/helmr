# Slack Approval

This example shows a Slack approval backed by Helmr session input. The bridge
starts one Helmr session with a Slack-shaped `externalId`, creates a narrow
public token that can append only to that session's `approval` input stream,
posts Slack buttons, and records the signed Slack action as durable session
input.

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

The bridge is intentionally stateless: each Slack button value carries a narrow,
single-use public token scoped to one session input stream. A production bridge
that does not want Slack to hold that capability can store the token server-side
and put only an opaque approval id in the Slack button value.

## Flow

1. The bridge starts a Helmr session with `externalId:
   slack:${SLACK_CHANNEL_ID}:${APPROVAL_RELEASE}`.
2. The task waits on `streams.input("approval").wait({ correlationId,
   timeout: "7d" })`.
3. The bridge creates a `session.input.send` public token scoped to that
   external-id-addressed session, the `approval` stream, and one correlation id.
4. The bridge posts Slack buttons whose value carries the external id and public
   token.
5. A signed Slack button click appends `{ approved, actor, channelId }` to the
   session input stream through `/api/v1/sessions/by-external-id/...`.
6. Repeated clicks receive a stable "already recorded" response after the public
   token or idempotency key has accepted the first decision.

If setup fails after the session starts, including public token creation or
Slack posting, the bridge cancels the parked session so the approval does not
sit waiting forever. By default the bridge derives Helmr idempotency keys from
the task, channel, and release; set `HELMR_START_IDEMPOTENCY_KEY` when you need
a different retry boundary.

The long approval timeout belongs to the stream wait, not to active task
compute. The task's `maxDuration` remains a short active budget because parked
stream waits release compute.

Use session input when the external system is part of the session timeline, as
Slack buttons, Slack messages, chat steering, browser controls, or progress
events are. Use tokens for one-shot completion that is independent of a session
stream, such as a standalone email link or provider callback. Use timers for
wall-clock waits without active compute budget.
