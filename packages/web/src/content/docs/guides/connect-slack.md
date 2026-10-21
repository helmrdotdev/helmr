---
title: Connect an Agent to Slack
description: Give a deployed Agent its own Slack app, link people's Slack accounts, and use channel threads or direct messages.
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

1. In the Console, select the Environment, open **Agents**, choose the Agent,
   and open its **Slack** tab. The tab walks you through the setup in four
   numbered steps.
2. Choose **Connect Slack**. Each Agent has one Slack connection per
   Environment.
3. Choose **Create app in Slack**. Slack opens with an app manifest already
   filled in: the app name, bot user, the scopes Helmr needs, and the event and
   interactivity URLs for this connection. The manifest is also shown in the
   Console if you prefer to paste it.
4. Create the app, then choose **I created the app** in the Console. From the
   app's **Basic Information** page in Slack, copy the **Client ID**,
   **Client Secret** and **Signing Secret**.
5. In the Console, enter the three values and choose **Save app credentials**.
   If Slack reported that the event URL could not be verified, retry the
   verification in Slack now.
6. Choose **Install app in Slack** and approve the installation.

Slack may show a token-rotation setup notice while creating the app. Keep token
rotation enabled; Helmr refreshes expiring installation tokens. The app is
configured to appear online in Slack. This indicator is not a server-health check.

The Slack tab shows the app and workspace once it is connected. From the same
tab you can update the app credentials, reauthorize the app, or disconnect it.
Before installation, **Discard setup** releases the Agent's connection without
deleting the app in Slack.

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

## Use direct messages

Open the app's Messages tab and send a request without a mention. Each new
message outside a thread starts a new Session. Reply in that message's thread
to continue; mentions are not needed in DM replies either. Replies to a closed
Session, an account-linking hint or a thread without an accepted Session do not
start new work. Send a new message outside the thread instead. An unbound reply
may wait up to five minutes for its root to be accepted before a hint is sent.

**DM records are shared under Helmr's existing Environment permissions.**
Authorized members can inspect the Session's input and output in Console even
though the Slack conversation is one-to-one. The app's description and Messages
tab guidance also explain this visibility. Group DMs and shared conversations
are not supported.

For an existing app, open **Update app manifest** in the Console's Slack tab.
Copy the current manifest into that same app's **App Manifest** settings in
Slack and save it. Then choose **Reauthorize in Slack** for the same workspace
to grant the DM permissions. Updating Helmr alone does not update the Slack app.
Existing channel conversations remain available before reauthorization. As with
any reauthorization, previously unsent output is not backfilled; finish active
conversations before updating the authorization.

## Link people's Slack accounts

Work requested in Slack runs as the Helmr user who asked. Each person links
their Slack account once per workspace:

- From Slack: the first time they request work or answer a question, the app
  sends them a link that asks them to sign in to Helmr and confirm both accounts.
  Their first action is not applied; they repeat it after linking. In a DM, send
  a new message outside the linking hint's thread.
- From the Console: open **Account settings** from the profile menu; **My
  account** lists linked identities under **Your Slack identity**. Choose a
  workspace and **Verify Slack identity**.

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
