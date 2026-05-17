---
title: Start control
description: Enable the Helmr control service and check health and readiness.
section: Self-hosting
sidebarLabel: Start control
order: 760
---

# Start control

After migrations pass, enable the control service:

```hcl
create_control_service = true
```

For the `standard` profile, confirm that the DNS name in `public_url` points at `control_load_balancer_dns_name` before using the service publicly.

Apply the change:

```sh
tofu apply
```

Check the public URL:

```sh
CONTROL_URL=$(tofu output -raw control_url)
curl -fsS "$CONTROL_URL/healthz"
curl -fsS "$CONTROL_URL/readyz"
```

Use `/healthz` for process liveness. Use `/readyz` for database and migration readiness. If `/healthz` passes but `/readyz` fails, check the database URL secret and migration task logs.

For initial setup, sign in with the account that matches `bootstrap_owner_email`. The setup flow is enabled for self-hosted deployments and creates the first owner account.
