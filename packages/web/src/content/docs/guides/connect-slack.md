---
title: Connect an Agent to Slack
description: Give a deployed Agent its own Slack app, link people's Slack accounts, and route Sessions to a channel.
---

# Connect an Agent to Slack

Each Agent connects to Slack through its own Slack app, so its name and avatar
identify it in conversations. Once connected, people mention the app to start
work, follow progress in the thread and answer the Agent's questions there. For
how Slack conversations map to Sessions, see [Slack](/docs/concepts/slack).

## Before you begin

- The Agent is deployed to the Environment you want to connect. See
  [Deploy](/docs/guides/deploy).
- You are an owner or admin of the organization. Connecting an Agent is done in
  the Console; there is no CLI command or `/v1` API for it.
- Slack can reach your control plane. The Slack app sends events and
  interactions to `PUBLIC_URL/integrations/slack/apps/…` and redirects sign-ins
  to `PUBLIC_URL/auth/slack/callback`, so the control plane's
  [`PUBLIC_URL`](/docs/reference/env) must be a public HTTPS origin. No other
  Slack settings are needed on the control plane.
- You can create apps in the Slack workspace.

## Create the Agent's Slack app

1. In the Console, select the Environment, open **Agents**, and choose the Agent.
   Under **Slack Connection**, choose **Manage Slack connection**.
2. Choose **Connect Slack**. Each Agent has one Slack connection per
   Environment.
3. Choose **Create app in Slack**. Slack opens with an app manifest already
   filled in: the app name, bot user, the scopes Helmr needs, and the event and
   interactivity URLs for this connection. The manifest is also shown in the
   Console if you prefer to paste it.
4. Create the app. From the app's **Basic Information** page in Slack, copy the
   **Client ID**, **Client Secret** and **Signing Secret**.
5. In the Console, enter the three values and choose **Save app credentials**.
   If Slack reported that the event URL could not be verified, retry the
   verification in Slack now.
6. Choose **Install app in Slack** and approve the installation.

The connection shows the app and workspace once it is connected.

## Use it in a channel

Invite the app to each channel where you want to use it, then mention it with a
request:

```text
/invite @reviewer
@reviewer Review issue 42
```

The mention starts a Session. The Agent's progress, questions and response
appear in the thread. To send more input, reply in the thread and mention the
app again; messages that do not mention the app are treated as discussion and
ignored.

## Link people's Slack accounts

Work requested in Slack runs as the Helmr user who asked. Each person links
their Slack account once per workspace:

- From Slack: the first time they request work or answer a question, the app
  sends them a link that asks them to sign in to Helmr and confirm both accounts.
  Their first action is not applied; they repeat it after linking.
- From the Console: **Slack account** in the account menu lists linked
  identities. Choose a workspace and **Verify Slack identity**.

Linking uses the person's existing Helmr permissions. Unlinking stops future
requests and answers from that Slack account; work already accepted continues.

## Route a Session started elsewhere to a channel

A Session started from the CLI, the SDK or a schedule can post into a Slack
channel of the connected workspace. Pass the channel's ID (for example
`C0123456789`, shown in Slack's channel details), not its name. The Agent's app
must be able to access the channel.

```sh
helmr session create reviewer --project demo --env staging \
  --slack-channel C0123456789 --text "Review issue 42"
```

```ts
await client.sessions.create({ agent: "reviewer", input: [{ type: "text", text: "Review issue 42" }],
  slack: { channelId: "C0123456789" }, })
```

For scheduled work, set `slack: { channelId }` on the cron trigger; see
[Triggers](/docs/concepts/triggers). The start fails if the Agent has no
connected Slack app or cannot access the channel.

## Repair or disconnect

If Slack authorization is lost, the connection asks for repair. Choose
**Reauthorize in Slack** for the same app and workspace; output that could not
be posted in the meantime is not posted afterward. To change the app's
credentials, use **Update app credentials**.

**Disconnect Slack** stops Slack input, answers and output in existing threads.
Sessions continue and remain available through the CLI and API. Reconnecting
creates a new connection: earlier threads stay disconnected, and triggers that
post to Slack take effect again only after a Deployment is promoted. Disconnecting
does not delete the app in Slack.
