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
it through your protected deployment process and open `/admin/login` in the Console. Redeem the capability, register a passkey and complete the separate key
assertion. Enrollment expires after fifteen minutes; unfinished registration
provides no operator authority. Remove the local capability file after use.

Operator sessions last twelve hours. Sensitive actions require confirmation within
the last hour. They do not authorize ordinary organization resources. To create
the first self-hosted organization, sign in separately with the ordinary account
that will own it, then complete setup with both identities. The deployment supports
one organization. An existing organization's owner is not promoted to operator.

To replace an operator's lost keys, use the same deployment command with `repair`
and `--operator-id`. For self-hosted ordinary account repair, use `repair-account`
with `--operator-id`, `--user-id` and a new protected `--output-file`; the operation
requires explicit `DEPLOYMENT_MODE=self-hosted`. The repair must complete verified
key registration before existing access is retired. It is not an email reset.

## Operator API authentication

Operator pages and APIs share the administration namespace: `/admin/login`,
`/admin/security` and `/admin/api/v1/*`. The internal operator identity remains
separate from ordinary accounts. Authentication is defined per operation, not
inferred from the URL prefix.

| Route under `/admin/api/v1` | Requirement |
| --- | --- |
| `POST /auth/options`, `/auth/verify` | Browser-bound challenge and a valid operator key; no existing operator session is required to sign in. |
| `POST /enrollment/redeem` | Valid deployment-issued enrollment capability in the body, bound to the redeeming browser. |
| `POST /enrollment/{id}/options`, `/complete`, `/assert/options`, `/assert/verify` | The matching restricted enrollment and browser binding; registration alone does not grant operator access. |
| `GET /me`, `/setup`, `/credentials` | Current operator session. |
| `POST /auth/renew/options`, `/auth/renew/verify` | Current operator session plus a fresh key assertion to complete renewal. Old confirmation does not prevent starting renewal. |
| `POST /auth/logout` | Retires the presented operator session; does not require a fresh confirmation. |
| `POST /credentials/enroll`, `DELETE /credentials/{id}` | Current operator session and the operation's fresh-key/retained-key checks. |
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
does not log out the others. Removing a sign-in method retires access derived from
that method; account-wide revocation retires all channels. Browser and CLI sessions
expire after thirty days without explicit human activity. Background polling does
not extend them. Removing an organization membership preserves the account and
other organizations, and reinvitation creates a new membership lifetime.

Use `helmr login` to authorize a CLI for the displayed account and organization.
Sensitive CLI commands request browser confirmation; `helmr login --confirm` starts
it explicitly. After confirmation, retry the original command yourself. A CLI token
cannot switch to another organization by adding a request header. Confirmation replaces
the old CLI session: queued work and open streams authorized by that session lose
access at their next authority check. Retry new work explicitly after confirmation.

Members with `members.remove` can revoke a member's sessions or suspend/resume the
member from **Members**. These operations affect only that organization, preserve
credentials and grants, and require fresh authentication before access resumes.
Owners cannot be suspended; resume a member before granting ownership. Account
settings and sign-in confirmation remain available when organization access ends.

A disconnected Slack installation is not an explicit unlink. Reauthorizing the
same installation can restore a still-valid link. Account or member revocation,
source removal and explicit unlink retire the old link; relinking creates new
authority and cannot revive work associated with the retired link.

## Invite members

Members with Manage access invite people from **Members** in the Console. Paste up to 50
email addresses at once, separated by commas, semicolons, spaces or new lines.
Invitees join without access; assign Roles directly or through Groups after they join.
Owners appoint other owners separately. Each invitee receives an
email with their invitation link, which expires after 7 days. An address that is
invalid, already belongs to an active member or already has a pending invitation
is reported without blocking the others. If the invitation emails cannot be
sent, no invitation is created and the Console lists the addresses that were not
invited. With SMTP delivery, a server that refuses one recipient partway through
a batch may already have accepted emails for the addresses before it; those
emails arrive, but their links don't work because no invitation was created.
Invite those people again.

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

## Upgrade an existing installation

The authentication migration preserves account, membership and audit attribution
IDs. It retires legacy browser/CLI sessions, pending device approvals, magic-link
credentials and Slack links that have no recorded proof source. Users must sign
in again and explicitly authorize CLI and Slack access. Existing organization
owners remain owners; the old account administrator flag never creates a deployment
operator or operator credential.

Before applying this upgrade, inventory the retained accounts, verified provider
subjects and login email addresses; resolve known historical account-correlation
errors separately. Back up the database and record the matching application and
worker artifact revisions. Quiesce human-authenticated work and prepare the
protected operator enrollment path before the coordinated cutover. Verify the
canonical browser origin and configured email/provider delivery on the target
installation, then test operator enrollment and ordinary owner access separately.

A rollback after new passkeys have been enrolled requires the pre-upgrade database
backup and its matching application artifacts. The down migration deliberately
refuses to discard those credentials. Do not treat a successful empty-database
round trip as approval to erase newly enrolled credentials or retained user work.
