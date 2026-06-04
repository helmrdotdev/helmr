---
title: Configure GitHub
description: Configure GitHub OAuth login for Helmr.
section: Self-hosting
sidebarLabel: Configure GitHub
order: 730
---

# Configure GitHub

Helmr uses GitHub OAuth for browser login. Repository access is not configured in the control plane; tasks that need GitHub should receive a scoped token as a task secret and perform repository operations inside the run.

Before the first apply, create a GitHub OAuth app and set this non-secret value in `terraform.tfvars`:

- `github_oauth_client_id`

After `tofu output control_url` is available, update the OAuth app callback URL:

| GitHub OAuth setting | Value |
| --- | --- |
| Callback URL | `<control_url>/auth/github/callback` |

Store the GitHub OAuth client secret in AWS Secrets Manager, not in Terraform variables:

- `github_oauth_client_secret`
