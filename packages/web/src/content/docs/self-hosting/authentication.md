---
title: Authentication
description: Configure GitHub OAuth and first-organization setup for a self-hosted environment.
---

# Authentication

Self-hosted Helmr uses a GitHub OAuth app for browser login. Repository credentials used by tasks are separate: pass a scoped credential as a task secret when a task needs repository access.

## Configure GitHub OAuth

Create the OAuth app before the first Terraform/OpenTofu apply and set its non-secret client ID:

```hcl
github_oauth_client_id = "your-client-id"
```

After the load balancer and public URL exist, set the OAuth callback URL to:

```text
<controlplane_url>/auth/github/callback
```

Use the exact `controlplane_url` output, including scheme and any configured hostname. Store the matching client secret in the stack-created `github_oauth_client_secret` Secrets Manager entry; never place it in tfvars.

## First organization

Self-hosted mode requires a high-entropy `setup_token`. The bootstrap helper generates it and stores it in Secrets Manager without writing it to Terraform state.

Once `/readyz` succeeds, sign in through GitHub. If the environment has no organization, enter the setup token to create the first organization. The self-hosted instance supports a single organization; after it exists, an owner must invite additional users.

The setup token is a bootstrap credential, not a substitute for user authentication. Restrict access to its secret and logs. The checked-in helper initializes missing secret values only; it does not rotate an existing setup token.
