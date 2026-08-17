---
title: Troubleshooting
description: Diagnose common Control Plane, authentication, dispatcher, worker, and upgrade failures.
---

# Troubleshooting

Start with the narrowest failing layer and use ECS service events, CloudWatch logs, Terraform/OpenTofu outputs, and SSM journals without copying secret values into logs or tfvars.

| Symptom | First checks |
| --- | --- |
| `tofu apply` cannot resolve release artifacts | Confirm `helmr_version`, the emitted manifest URL, HTTP access to the manifest, a digest-pinned `controlplane_image`, and a worker AMI entry for `aws_region` when workers are enabled. |
| Database bootstrap or migration task fails | Wait for the ECS task's container exit code, then inspect its CloudWatch logs. Check task subnets, public-IP mode or NAT, security groups, the application database URL, the RDS-managed master secret for bootstrap, and ClickHouse reachability for migrations. |
| `/healthz` fails | Confirm the Control Plane service has running tasks and the ALB target and public DNS route to the service. Inspect ECS events and task logs. |
| `/readyz` fails while `/healthz` passes | Check `DATABASE_URL`, `REDIS_URL`, RDS and Redis/Valkey reachability, and whether migrations for the deployed image completed successfully. |
| GitHub login fails | The callback must be exactly `<controlplane_url>/auth/github/callback`; verify the client ID and that the secret matches the same OAuth app. |
| Run stays queued | Confirm a dispatcher task is running, desired worker capacity is nonzero, and at least one compatible execution Worker is active. |
| Worker does not become active | Run `helmr-worker status`; inspect its systemd journal through SSM. Check KVM, Firecracker, jailer, `ip`, `nft`, certified guest artifacts, the enrollment-token file, Pool name, capacity settings, and Control Plane reachability. |
| Worker launch or drain stalls | Check the Auto Scaling lifecycle hook, worker unit status, active executions, launch timeout, and whether provider scaling attempted to bypass protected, claim-fenced draining. |
| Task cannot reach a repository or API | Check the task secret and its scope, task configuration, worker NAT or egress, DNS, and blocked network policy. |
| Bundle build fails | Run `helmr build` locally and inspect the isolated builder output, lockfile, dependency credentials, and available local or CI disk. |
| Checkpoint resume fails after replacement | Check checkpoint encryption-key availability and compatibility of backend, architecture, ABI, kernel, rootfs, runtime configuration, vCPU, memory, and network ABI. |
| Service fails after an upgrade | Compare the resolved image and AMI with the recorded release, verify migrations ran before service rollout, and follow the prepared rollback plan. An image downgrade alone does not reverse schema changes. |

Useful outputs include:

```sh
tofu output controlplane_url
tofu output controlplane_cluster_name
tofu output controlplane_service_name
tofu output dispatcher_service_name
tofu output migration_task_definition_arn
tofu output -json secret_arns
tofu output controlplane_image
tofu output worker_ami_id
tofu output release_artifacts_manifest_url
```

When reading or updating a secret, address it by ARN from the outputs. Do not print its value into a diagnostic transcript.
