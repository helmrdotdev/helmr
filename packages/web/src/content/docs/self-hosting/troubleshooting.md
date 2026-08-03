---
title: Troubleshooting
description: Common setup failures and the first checks to make.
section: Self-hosting
sidebarLabel: Troubleshooting
order: 800
---

# Troubleshooting

| Symptom | First checks |
| --- | --- |
| `/healthz` fails | Control Plane task is not running, load balancer target is unhealthy, or the public URL does not route to the service. |
| `/readyz` fails | Database URL or `HELMR_REDIS_URL` is wrong, RDS or Redis/Valkey is unavailable, or migrations have not run successfully. |
| GitHub login fails | Callback URL must be `<controlplane_url>/auth/github/callback`; OAuth client secret must match the OAuth app. |
| Run stays queued | Dispatcher is not running, Redis/Valkey is unavailable, no active workers exist, desired capacity is zero, worker enrollment failed, or worker cannot reach the Control Plane. |
| Worker does not activate | Check KVM, Firecracker, jailer, `ip`/`nft`, certified guest artifacts, the group enrollment secret file, requested roles, and outbound network access. |
| External repository access fails | Check the task secret, token scope, payload values, and worker egress. |
| Image build fails | Check the certified guest-rootfs digest, image-build guest result, and guest egress to registries and AWS APIs. BuildKit runs inside the fresh image-build guest, not as a worker-host service. |
| Parked wait resume fails | Check worker availability and checkpoint runtime compatibility. |

For Control Plane or dispatcher task failures, inspect ECS service events and CloudWatch logs for the affected ECS service. For Worker failures, use SSM to inspect systemd journals on the Worker instance.

Do not debug by copying secrets into Terraform files. Read the secret ARN from `tofu output -json secret_arns`, then update the value in Secrets Manager.
