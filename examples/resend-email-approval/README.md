# Resend Email Approval

This example shows an email approval surface backed by a Helmr token
and Resend. Helmr owns the durable session, run, and token; the
app owns email delivery, sender identity, and recipient routing.

## Deploy and Start a Session

```sh
helmr deploy examples/resend-email-approval
helmr session start resend-email-approval --json \
  --payload-json '{
    "release": "helmr-web-2026-06-14",
    "summary": "Promote the validated staging build to production.",
    "risk": "Touches run input delivery.",
    "stagingUrl": "https://staging.example.com",
    "productionUrl": "https://example.com"
  }'
```

Start the bridge:

```sh
export HELMR_API_URL="https://dev.helmr.dev"
export HELMR_API_KEY="..."
export PUBLIC_BASE_URL="https://your-bridge.example.com"
export RESEND_API_KEY="re_..."
export RESEND_FROM="Helmr <approvals@your-domain.example>"
export EMAIL_TO="reviewer@example.com"
bun run --cwd examples/resend-email-approval bridge
```

`PUBLIC_BASE_URL` must be reachable by the email recipient because the approval
and rejection links open confirmation pages on the bridge. The confirmation
page records the response only after the recipient submits the form; scanners
that only fetch links do not approve or reject the wait.

## Flow

1. The task creates a token and waits with `tokens.wait(token)`.
2. The bridge polls pending tokens tagged `bridge:resend-email-approval`.
3. The bridge sends an email through Resend backed by the token.
4. The recipient opens approve or reject and submits the confirmation form.
5. The bridge completes the token.

The task contains no email-specific SDK code. Resend is just one delivery
surface for the generic Helmr token.

Tokens are bearer-equivalent capabilities for one pending wait.
Treat email links as sensitive. This example keeps delivered wait ids in memory
for clarity; a production bridge should persist provider message ids so restarts
do not resend the same pending wait.

## Resend API Shape

The bridge sends:

```ts
await fetch("https://api.resend.com/emails", {
  method: "POST",
  headers: {
    authorization: `Bearer ${process.env.RESEND_API_KEY}`,
    "content-type": "application/json",
  },
  body: JSON.stringify({
    from: process.env.RESEND_FROM,
    to: [process.env.EMAIL_TO],
    subject,
    html,
    text,
  }),
})
```

This mirrors Resend's email send API while keeping Helmr integration code
adapter-free.
