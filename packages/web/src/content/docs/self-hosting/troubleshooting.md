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
| `/healthz` fails | Control task is not running, load balancer target is unhealthy, or the public URL does not route to the service. |
| `/readyz` fails | Database URL or `HELMR_REDIS_URL` is wrong, RDS or Redis/Valkey is unavailable, or migrations have not run successfully. |
| GitHub login fails | Callback URL must be `<control_url>/auth/github/callback`; OAuth client secret must match the GitHub App. |
| GitHub webhooks fail | Webhook URL must be `<control_url>/webhooks/github`; webhook secret must match the value in Secrets Manager. |
| Run stays queued | Dispatcher is not running, Redis/Valkey is unavailable, no active workers exist, desired capacity is zero, worker registration failed, or worker cannot reach the control plane. |
| Worker does not activate | Check KVM, Firecracker, jailer, CNI, BuildKit, guest artifacts, registration token, and outbound network access. |
| Checkout fails | GitHub App is not installed on the repository, or the project points at the wrong repository/ref. |
| Image build fails | Check BuildKit service status and worker egress to registries and AWS APIs. |
| Waitpoint resume fails | Check worker availability and checkpoint runtime compatibility. |

For control or dispatcher task failures, inspect ECS service events and CloudWatch logs for the affected ECS service. For worker failures, use SSM to inspect systemd journals on the worker host.

Do not debug by copying secrets into Terraform files. Read the secret ARN from `tofu output -json secret_arns`, then update the value in Secrets Manager.
