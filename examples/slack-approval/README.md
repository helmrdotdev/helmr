# Slack Approval

This example shows a Slack-owned approval surface backed by Helmr session input.
Helmr owns the durable task session; the trusted Slack bridge owns Slack app
configuration, channel routing, and actor identity.

## Deploy and Start a Session

```sh
helmr deploy examples/slack-approval
helmr run slack-approval \
  --payload-json '{
    "release": "helmr-web-2026-06-14",
    "summary": "Promote the validated staging build to production.",
    "risk": "Touches run input delivery.",
    "stagingUrl": "https://staging.example.com",
    "productionUrl": "https://example.com"
  }'
```

Copy the printed session id and start the bridge:

```sh
export HELMR_API_URL="https://dev.helmr.dev"
export HELMR_API_KEY="..."
export HELMR_SESSION_ID="session_..."
export SLACK_BOT_TOKEN="xoxb-..."
export SLACK_SIGNING_SECRET="..."
export SLACK_CHANNEL_ID="C0123456789"
export APPROVAL_TITLE="Approve helmr-web-2026-06-14?"
export APPROVAL_SUMMARY="Promote the validated staging build to production."
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

1. The task opens an `approval` session input channel and waits for a record.
2. The trusted bridge posts Slack buttons for `HELMR_SESSION_ID`.
3. A button click verifies the Slack signature.
4. The bridge appends the approval record with its Helmr API key.

The task contains no Slack-specific SDK code. Slack is just one delivery surface
for the generic Helmr session input channel.

The bridge is a trusted endpoint. Keep its Helmr API key server-side and verify
Slack signatures before appending session input. This example posts one message
on startup for clarity; a production bridge should persist Slack message ids so
restarts do not repost the same request.
