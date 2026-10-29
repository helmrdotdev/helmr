# Helmr AWS Quickstart

A small self-hosted installation for trying Helmr or continuing to use it with a
small team. One EC2 host runs Control Plane, Dispatcher, PostgreSQL, ClickHouse
and Caddy HTTPS. A separate EC2 Worker runs agents. There is no ALB, NAT gateway,
Fargate or RDS, and no automatic Worker scaling.

Database, telemetry and TLS state live on the control host's encrypted root disk.
They survive service restarts but are deleted when the host is replaced or
terminated. **There are no backups.** Application artifacts use the stack's S3
CAS; destroying the stack also deletes that bucket's contents. Standard remains
the separate-service configuration with managed PostgreSQL and backup defaults.

## Inputs

Use OpenTofu/Terraform and your normal AWS credentials. Supply:

- A regional Ubuntu 24.04 LTS **amd64** control-host AMI with cloud-init and SSM
  Agent, and a matching Helmr Worker AMI prepared from the selected release's
  host/runtime bundles. The shared [Worker image stack](../stacks/worker-image/README.md)
  remains available; Quickstart does not build images.
- The released Control Plane image by digest and the matching `helmr_version`.
- Existing Platform Artifact store URI, bucket ARN and key ARN, with the selected
  signed Runtime already published as described in the
  [release requirements](../../../packages/web/src/content/docs/self-hosting/requirements.md#release-artifacts).
- A public hostname you can point directly at the emitted IPv4 address, and a
  GitHub OAuth app. Its callback is `https://your-host/auth/github/callback`.

ClickHouse is included. No separate telemetry account or ACM certificate is
needed. Caddy obtains and renews HTTPS certificates; DNS must resolve directly to
this host, with TCP 80 and 443 reachable. PostgreSQL, ClickHouse and the underlying
Control Plane listener bind only to loopback. Host administration uses SSM.

## Create and start

```sh
cp terraform.tfvars.example terraform.tfvars
# Fill in the deployment-specific values; leave create_worker=false initially.
tofu init
tofu apply
```

This creates infrastructure and installs the container configuration. Secret
values are not written to Terraform state or EC2 user data. Point your hostname's
A record at `tofu output -raw controlplane_public_ip`. Do not add an AAAA record
or a proxy/CDN in front of this example.

Provide `HELMR_GITHUB_OAUTH_CLIENT_SECRET` through your ordinary secret input, then
populate the empty Secrets Manager entries with the existing helper:

```sh
../../../scripts/aws-bootstrap-helmr-secrets.sh
aws ssm start-session --target "$(tofu output -raw control_instance_id)"
```

On the control host:

```sh
sudo cloud-init status --wait
sudo systemctl enable --now helmr.service
sudo systemctl enable --now helmr-audit.timer
```

The service reads application secrets from Secrets Manager, keeps generated
local database passwords in `/var/lib/helmr/database-credentials.json`, checks
configuration, bootstraps separate database/telemetry roles and runs migrations
before starting Helmr. Root-only `/run/helmr/compose.json` contains the materialized
container configuration; do not copy it into logs or support bundles.
The timer performs ordinary audit-record retention; it does not back up data.

Check `https://your-host/readyz`, enroll a deployment operator through the protected
local command, then sign in separately as the first organization owner. See the
[authentication guide](../../../packages/web/src/content/docs/self-hosting/authentication.md).

Then set `create_worker=true` and apply again. The default is one `c8i.large`
Worker with a 320-GiB disk and one 1-vCPU/2-GiB execution slot. Use an Agent
Template that fits that shape, or change the Worker inputs. This initial size is
not a workload-performance guarantee.

Both hosts have public outbound connectivity. The Worker opens no inbound ports.
Guest egress denies include the VPC, metadata and the control host's public
address; application authentication and the existing Worker isolation remain.
Control-host containers share one host identity, unlike separate ECS task roles;
that role has scoped store/secret access, not EC2 or Auto Scaling mutation rights.

## Everyday operations

On the control host:

```sh
sudo systemctl status helmr.service
sudo journalctl -u helmr.service
sudo docker compose -f /run/helmr/compose.json ps
sudo systemctl restart helmr.service
```

Restart reuses local database credentials and data, reruns idempotent bootstrap
and migrations, and reloads Secrets Manager values. The configured image digest
is unchanged. Do not run multiple startup/migration commands concurrently.

Changing control-host bootstrap inputs, including the Control Plane image or
process configuration, replaces the host and deletes its local data. This is
**not an in-place upgrade**. For a new release, finish or cancel work, drain and
remove Workers while the old Control Plane is still available, then recreate
the installation and redeploy your agents. Do not apply this root over an older
Fargate/RDS Quickstart state expecting a migration.

Worker-only updates retain the existing immutable Pool generations and
[full-stop capacity procedure](../../../packages/web/src/content/docs/self-hosting/upgrades.md).
Keep the current Control Plane inputs unchanged during that procedure. The
Capacity token is initialized by the helper and its ARN is emitted as
`capacity_token_secret_arn`. Enroll a separate deployment operator through the
protected local command before Pool retirement is needed.

## Remove

Finish or cancel work, then drain and remove Workers using the same full-stop
procedure while Control Plane and networking are still available. Destroy the
remaining stack with `tofu destroy`. The separate Platform Artifact foundation
is outside this root. Normal AWS KMS deletion waits still apply.

This root has no backend block; add your normal backend configuration if needed.
Keep your deployment's generated provider lock file. Quickstart is infrastructure
configuration, not an account setup or release-publication tool.
