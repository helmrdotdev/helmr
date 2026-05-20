---
title: Start control
description: Enable the Helmr control and dispatcher services and check health and readiness.
section: Self-hosting
sidebarLabel: Start control
order: 760
---

# Start control

After migrations pass, enable the control and dispatcher services:

```hcl
create_control_service = true
control_desired_count = 1
dispatcher_desired_count = 1
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

Use `/healthz` for process liveness. Use `/readyz` for database, Redis/Valkey, and migration readiness. If `/healthz` passes but `/readyz` fails, check the database URL secret, `HELMR_REDIS_URL`, and migration task logs.

For initial setup, sign in with the account that matches `bootstrap_owner_email`. The setup flow is enabled for self-hosted deployments and creates the first owner account.
