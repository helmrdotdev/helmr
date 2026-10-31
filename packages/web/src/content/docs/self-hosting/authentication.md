---
title: Authentication
description: Configure sign-in, deployment operators, and first-organization setup.
---

# Authentication

Helmr supports passkeys, email verification codes when delivery is configured, and GitHub OAuth when configured. Repository credentials used by Agents are separate: bind a scoped Secret to the Agent’s Computer when it needs repository access.

## Configure GitHub OAuth

Create the OAuth app before the first Terraform/OpenTofu apply and set its non-secret client ID:

```hcl
github_oauth_client_id = "your-client-id"
```

After the application endpoint and public URL exist, set the OAuth callback URL to:

```text
<controlplane_url>/auth/github/callback
```

Use the exact `controlplane_url` output, including scheme and any configured hostname. Store the matching client secret in the stack-created `github_oauth_client_secret` Secrets Manager entry; never place it in tfvars.

## First organization and deployment operators

Deployment operators are separate from ordinary accounts and organization owners.
Use a protected deployment shell with the application database connection already
configured to issue a short-lived enrollment file:

```sh
control-plane operator enroll --label "Deployment operator" --output-file /protected/operator-enrollment.json
```

The command creates a new owner-readable file and never prints the capability.
Keep it out of arguments, URLs, logs, source control and support bundles. Transfer
it through your protected deployment process and open `/admin/login` in the Console. Redeem the capability and register a passkey; registration installs
it as the operator's key. Enrollment expires after fifteen minutes; unfinished
registration provides no operator authority. Remove the local capability file
after use. If the new key cannot sign in, issue a new capability with `repair`.

Operator sessions last twelve hours. Confirming your sign-in with an operator key
keeps the session and gives it twelve hours from that confirmation. Sensitive
actions require confirmation within the last hour. They do not authorize ordinary organization resources. To create
the first self-hosted organization, sign in separately with the ordinary account
that will own it, then complete setup with both identities. The deployment supports
one organization. An existing organization's owner is not promoted to operator.

To replace an operator's lost keys, use the same deployment command with `repair`,
`--operator-id` and a new protected `--output-file`; the operator keeps its label. For self-hosted ordinary account repair, use `repair-account`
with `--operator-id`, `--user-id` and a new protected `--output-file`; the operation
requires explicit `DEPLOYMENT_MODE=self-hosted`. Ordinary account repair completes
when the account holder registers a new key. Existing browser, CLI and Slack access is revoked; accepted work continues.
Existing sign-in methods remain for the holder to review.
Operator key replacement installs the newly registered key, retires the
operator's other keys and signs out its sessions. Neither operation is an email
reset.

Accepted Sessions, Turns and Commands continue after logout, credential removal,
account revocation, membership removal or API-key expiration. New requests,
human answers, management actions and result disclosure require current access.
An Agent executes within its published Deployment contract, pinned when the
Session starts; it does not borrow the current permissions of its original caller
or a later participant. A currently authorized colleague can continue the existing
conversation.

Revoke account or member access to prevent new operations and disclosure. Stop
unwanted work separately with Session and Command controls. **View your sessions**
in Sign-in and security, or **View related work** in Members, shows visible work
initiated by or involving that person, plus descendants, in the selected Environment. It does not include work
you cannot view or turn revocation into automatic cancellation. Schedules and
organization-owned resources remain independently managed. A valid sign-in can
establish new access after revoke-all; retire compromised credentials as well.

Each session records the sign-in method and time it signed in with. Removing a
sign-in method stops it from signing in again; browsers, CLI logins and Slack
links that already signed in with it stay signed in. Use **Sign out other
sessions** in Sign-in and security to end every session except the browser you
are using, or **Sign out everywhere** to end this browser too. Neither needs a
recent sign-in. Your last usable sign-in method cannot be removed: a passkey, a
linked provider the deployment still offers, or email codes to your login address
when email delivery is set up.

## Operator API authentication

Operator pages and APIs share the administration namespace: `/admin/login`,
`/admin/operator-keys`, `/admin/account-repair` and `/admin/api/v1/*`. The internal operator identity remains
separate from ordinary accounts. Authentication is defined per operation, not
inferred from the URL prefix.

| Route under `/admin/api/v1` | Requirement |
| --- | --- |
| `POST /auth/options`, `/auth/verify` | Browser-bound challenge and a valid operator key; no existing operator session is required to sign in. |
| `POST /enrollment/redeem` | Valid deployment-issued enrollment capability in the body, bound to the redeeming browser. |
| `POST /enrollment/{id}/options`, `/complete` | The matching restricted enrollment and browser binding. Completing registration installs the key and signs out the operator's sessions; it does not sign in. |
| `GET /me`, `/setup`, `/credentials` | Current operator session. |
| `POST /auth/options`, `/auth/verify` with an operator session | Confirms the current session with a fresh key assertion and extends it to twelve hours from that confirmation. Old confirmation does not prevent starting it. |
| `POST /auth/logout` | Retires the presented operator session; does not require a fresh confirmation. |
| `POST /credentials/options`, `/credentials/complete` | Current operator session confirmed within the last hour at both steps; the registration is bound to that session and browser. |
| `DELETE /credentials/{id}` | Current operator session confirmed within the last hour, signed in with a key other than the one being removed. |
| `POST /account-repairs/*` | Self-hosted only; current operator session plus the repair capability, browser binding and operation-specific fresh-proof checks. |
| Existing region, worker-group and pool APIs | Current operator session; mutations additionally require recent operator proof. |

All these browser mutations have same-origin protection. Ceremony requests have
a 128 MiB request limit; infrastructure requests have a 64 KiB limit.
Operator cookies use root path scope so the first
organization can be created through `POST /api/organizations` with both independent
identities. Cookie path is not an authorization boundary.

The Console returns to the requested page after operator login. Setup displays an
operator sign-in action when that session is absent, asks for the ordinary owner
account when needed, and offers retry for service or network failures.

## Ordinary accounts and CLI

Each account can hold multiple passkeys and linked sign-in methods. Email codes
are entered in the browser that requested them; invitation URLs alone do not
prove mailbox ownership. A provider's reported email does not silently change
an account's login email or merge accounts.

Browser, CLI and Slack access have separate lifetimes. Logging out of one channel
does not log out the others, and removing a sign-in method logs out none of them.
Signing out other sessions retires every channel except this browser; signing out
everywhere and changing your login email also end the others. Browser sessions
expire after thirty days without explicit human activity. A CLI login expires
thirty days after its last request. Removing an organization membership preserves
the account and other organizations, and reinvitation creates a new membership
lifetime.

Changes that alter access or destroy something require a sign-in confirmed within
the last hour: Roles and assignments, ownership, Group membership changes for
Groups that carry assignments, API keys, rotating or revoking Secrets, removing
members, deleting Groups and connections, deleting or restoring Agents, updating or
deleting Computers, shortening Session history retention, and adding or removing
sign-in methods and changing your login email. Other changes, such as creating a
Secret, setting up a Slack connection, lengthening retention, changing your display
name or signing out, use your current sign-in. When confirmation is needed, the
Console asks you to confirm your sign-in and then retry.

Use `helmr login` to authorize a CLI for the displayed account and organization.
Approving a CLI login needs a browser that signed in within the last hour; the
approval page asks you to confirm your sign-in first when it is older. That
approval is the CLI's proof, so CLI commands never ask for confirmation. A CLI
token cannot switch to another organization by adding a request header.

Members with `members.remove` can revoke a member's sessions or suspend/resume the
member from **Members**. These operations affect only that organization and
preserve credentials and grants. Revoking sessions or suspending signs the member
out of the organization, retires their CLI logins, device approvals and Slack links,
and requires a sign-in after that point before access returns. Resuming only lifts
the suspension; the member signs in again if they have not since the suspension.
Owners cannot be suspended; resume a member before granting ownership. Account
settings and sign-in confirmation remain available when organization access ends.

A disconnected Slack installation is not an explicit unlink. Reauthorizing the
same installation can restore a still-valid link. Account or member revocation
and explicit unlink retire the old link. Relinking creates new access. Resuming
private delivery to earlier conversations requires explicit consent, the same
Helmr account and Slack recipient, and current access. Removing a sign-in method
does not revoke an issued link; signing out other sessions or everywhere does. Disconnecting Slack stops input and delivery;
accepted computation and result storage continue.

## Invite members

Members with Manage access invite people from **Members** in the Console. Paste up to 50
email addresses at once, separated by commas, semicolons, spaces or new lines.
Invitees join without access; assign Roles directly or through Groups after they join.
Owners appoint other owners separately. Each invitee receives an
email with their invitation link, which expires after 7 days. Each address is
invited on its own: one that is invalid, already belongs to an active member,
already has a pending invitation or whose email cannot be sent is reported
without blocking the others. An address whose email cannot be sent gets no
invitation; the Console lists it with the reason, and you can invite it again.

Inviting members requires [email delivery](/docs/reference/env). Without it,
members can't be invited.

## Deployment maintenance

Operator sessions authorize the separate Pool retirement API used after
[maintenance](/docs/self-hosting/upgrades#retire-an-old-pool-separately).
Keep the deployment Capacity token separate; it does not grant operator-session
or ordinary account access.

## Browser origin

Set `APP_ORIGIN` to the canonical HTTPS DNS origin that users and deployment
operators open in their browser. IP-literal and single-label deployment origins
are unsupported because operator bootstrap and repair require WebAuthn. For local
development, use `http://localhost:<port>`; the server may still listen on loopback.
The control plane rejects an unsupported origin at startup rather than starting
without its required operator authentication path.

## Database initialization and retained installations

These pre-release schema sources are revised together and require a freshly
initialized database. In-place upgrades from earlier pre-release schemas are not
supported. Migration tests against earlier versions of the current source do not
qualify databases created by an older release.

A retained installation needs a separately qualified migration and recovery plan.
Inventory its accounts, provider subjects, login addresses and Slack history;
retain a backup and matching application and worker artifacts before any data
operation. Existing Slack threads and accepted inputs do not have the required
consent/source references, and existing constraint names can differ. No automatic
backfill or reset is provided. Existing organization ownership never creates a
deployment operator credential.

The down migration refuses to discard enrolled passkeys or undelivered security
notifications. A successful empty-database round trip does not authorize erasing
credentials, pending delivery or retained user work. Qualify recovery with the
installation's data and matching application artifacts.
