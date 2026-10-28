---
title: Authentication
description: Configure GitHub OAuth and first-organization setup for a self-hosted environment.
---

# Authentication

Self-hosted Helmr uses a GitHub OAuth app for browser login. Repository credentials used by Agents are separate: bind a scoped Secret to the Agent’s Computer when it needs repository access.

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

## First organization

Self-hosted mode requires a high-entropy `setup_token`. The bootstrap helper generates it and stores it in Secrets Manager without writing it to Terraform state.

Once `/readyz` succeeds, sign in through GitHub. If the environment has no organization, enter the setup token to create the first organization. The self-hosted instance supports a single organization; after it exists, a member with Manage access must invite additional users.

The setup token is a bootstrap credential, not a substitute for user authentication. Restrict access to its secret and logs. The checked-in helper initializes missing secret values only; it does not rotate an existing setup token.

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

## Deployment administrators

The AWS reference roots accept `controlplane_environment.ADMIN_EMAILS`, a comma-separated list of verified login emails
that receive deployment administrator authority when those users first sign in.
Set it before the intended operators' first login. It does not promote existing
users or revoke existing grants when edited. Organization ownership and deployment
administrator authority are separate. Administrator sessions authorize the
fenced Pool retirement API used after [maintenance](/docs/self-hosting/upgrades#retire-an-old-pool-separately);
the deployment Capacity token does not grant administrator-session access.
