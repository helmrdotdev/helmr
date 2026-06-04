---
title: Deploy on AWS
description: Create the base AWS infrastructure from the AWS profiles.
section: Self-hosting
sidebarLabel: Deploy on AWS
order: 720
---

# Deploy on AWS

The repository provides two AWS profiles under `infra/aws/quickstart` and `infra/aws/standard`. Treat them as starting points, not as separate product modes.

| Profile | Use it when | Default shape |
| --- | --- | --- |
| `quickstart` | You want the smallest path to a control-plane evaluation. | CloudFront default domain, NAT disabled, control/dispatcher tasks with public IPs, workers disabled, lower-cost RDS defaults. |
| `standard` | You want the production baseline for a customer environment. | Public HTTPS ALB, private control, dispatcher, and migration tasks, NAT enabled, two AZs, stronger RDS backup and deletion defaults. |

Do not use `quickstart` as the production baseline. If an evaluation becomes a customer environment, create the production environment from `standard` and migrate deliberately instead of gradually accumulating production requirements in `quickstart`.

Start from the profile that matches your target environment:

```sh
cd infra/aws/quickstart
# or
cd infra/aws/standard

cp terraform.tfvars.example terraform.tfvars
```

Fill the non-secret values in `terraform.tfvars`, including:

- AWS region and environment name.
- `helmr_version`.
- `github_oauth_client_id`. Create the OAuth app before the first apply so the client ID is available.
- Public URL and certificate settings when you use your own domain.
- Optional email sender settings such as `email_provider` and `email_from`.

Keep `create_control_service = false` for the first apply. The control and dispatcher services need secret values and database migrations before they can become ready.

```sh
tofu init
tofu apply
```

After the first apply, record these outputs:

```sh
tofu output control_url
tofu output control_load_balancer_dns_name
tofu output redis_url
tofu output -json secret_arns
```

Use `control_url` as the externally reachable base URL for GitHub callbacks, CLI login, and browser access.

For the `standard` profile, point your DNS name at `control_load_balancer_dns_name` before relying on `control_url`. The ACM certificate for `public_url` must be in the same AWS region as the ALB.

For Resend email delivery, configure:

```hcl
email_provider = "resend"
email_from     = "Helmr <noreply@example.com>"
```

After applying, populate the emitted `secret_arns.resend_api_key` Secrets Manager secret with the
Resend API key before starting the control service.
