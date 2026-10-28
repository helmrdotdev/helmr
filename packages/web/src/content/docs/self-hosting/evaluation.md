---
title: AWS Quickstart
description: Run a compact self-hosted Helmr installation with local PostgreSQL and ClickHouse.
sidebarLabel: AWS Quickstart
---

# AWS Quickstart

`infra/aws/quickstart` provides a small installation for evaluation or ongoing
use. One EC2 host runs Control Plane, Dispatcher, PostgreSQL, ClickHouse and
HTTPS; a separate Worker runs agents. It uses released images and existing
Platform Artifact storage. There is no ALB, NAT gateway, RDS or Fargate.

**There are no backups.** Database and telemetry data survive ordinary restarts
but are deleted when the control host is replaced or terminated.

## Configure

```sh
cd infra/aws/quickstart
cp terraform.tfvars.example terraform.tfvars
```

Set your Region, hostname, GitHub OAuth client ID, regional Ubuntu 24.04 amd64
control AMI, released Control Plane image digest, matching Worker AMI, and the
existing Platform Artifact store inputs. The selected signed Runtime must already
be published in that store; see [requirements](/docs/self-hosting/requirements).
Keep secret values out of tfvars. ClickHouse is included and Caddy manages TLS,
so this profile does not require external ClickHouse or an ACM certificate.

Keep `create_worker=false` for the first apply:

```sh
tofu init
tofu apply
```

Point the application's DNS A record directly at `controlplane_public_ip`.
Provide `HELMR_GITHUB_OAUTH_CLIENT_SECRET` through your normal secret input, then:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
aws ssm start-session --target "$(tofu output -raw control_instance_id)"
```

On the host:

```sh
sudo cloud-init status --wait
sudo systemctl enable --now helmr.service
sudo systemctl enable --now helmr-audit.timer
```

Startup checks configuration, initializes PostgreSQL/ClickHouse roles and runs
migrations before starting Helmr. Check `https://your-host/readyz`, sign in and
complete [first-organization setup](/docs/self-hosting/authentication) with the
setup token. Then set `create_worker=true` and apply again. The default Worker
has one 1-vCPU/2-GiB execution slot; use an Agent Template that fits it.

## Operate

Use `sudo systemctl restart helmr.service` to restart with existing data and
reload secrets. Inspect `sudo journalctl -u helmr.service` or
`sudo docker compose -f /run/helmr/compose.json ps` on the host.
The rendered Compose file contains secrets and must remain root-only.

Changes to control-host bootstrap inputs replace the host and delete local data;
this is not an in-place upgrade. Drain and remove Workers while the old Control
Plane is available before recreating the installation. Worker-only maintenance
uses the [full-stop procedure](/docs/self-hosting/upgrades) without changing
Control Plane inputs. For removal, drain and remove Workers first, then use
`tofu destroy` for the remaining stack. Its CAS data is deleted too.

The profile has no backend block; use your normal backend when needed.
The checked-in `infra/aws/quickstart/README.md` describes the inputs and commands.
