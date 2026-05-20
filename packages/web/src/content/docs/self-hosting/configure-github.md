---
title: Configure GitHub
description: Configure the GitHub App URLs and credentials used by Helmr.
section: Self-hosting
sidebarLabel: Configure GitHub
order: 730
---

# Configure GitHub

Helmr uses a GitHub App for repository access, OAuth, and webhook verification.

Before the first apply, create the GitHub App and set these non-secret values in `terraform.tfvars`:

- `github_app_id`
- `github_app_slug`
- `github_app_client_id`

After `tofu output control_url` is available, update the GitHub App URLs:

| GitHub App setting | Value |
| --- | --- |
| Callback URL | `<control_url>/auth/github/callback` |
| Webhook URL | `<control_url>/webhooks/github` |

Store these GitHub App secrets in AWS Secrets Manager, not in Terraform variables:

- GitHub App private key PEM.
- Webhook secret.
- OAuth client secret.

Make sure the GitHub App is installed on every organization or repository that Helmr should be able to run against. If a run cannot read a repository, check the App installation before checking worker behavior.
