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

Keep at least one dispatcher task running when schedules are used. The dispatcher repairs schedule next-fire entries from the database into Redis/Valkey, leases due fires, and creates scheduled runs.

For initial setup, populate `HELMR_SETUP_TOKEN`, then sign in at the public URL. If no organization exists yet, Helmr sends the signed-in user to `/organizations/new` and asks for the setup token. After the first organization is created, Helmr sends the owner to `/projects/new` for the first project. The first project automatically gets a default `Production` environment.

Self-hosted Helmr uses one organization per instance. After the first organization exists, users who are not members cannot create another organization; an owner must invite them.
