# Private application ingress with restricted platform egress

This reference composes the same Control Plane and Worker modules as Standard. Quickstart shares the Worker
module but runs control services on one host. It keeps Console/API ingress on an internal HTTPS ALB and uses
Socket Mode for Slack. It is a starting point for an experienced network operator;
run the qualification below before using it for customer work.

## Responsibilities and prerequisites

The deployment operator owns DNS, certificates, routing, platform egress, service
endpoints, capacity and process settings. Organization owners/admins own each
Agent's Slack app and its encrypted credentials in Console. End users sign in and
link their Slack identity through a browser connected to the company network.

Provide an existing company VPN/Direct Connect/transit path, a trusted ACM
certificate for `app_hostname`, a private ClickHouse endpoint and its three
purpose-specific secret ARNs, a verified Product release image mirrored into
regional ECR, the Platform Artifact store, and a compatible Worker AMI. Mirror
the complete manifest listed in `proxy-image.json` into ECR without changing its
digest. The reference uses Canonical's pinned Squid 6.14 image; the source tag is
informational and is never used at runtime. Proxy replacement requires the same
qualification, including CONNECT/WSS and deny behavior. The task uses the image's
UID-13-owned `/var/spool/squid` volume for its configuration and PID; the declared
volume path preserves ownership on ECS. Decision logs include only the destination
host/port, method, status and sizes, never URL paths, query strings or credentials.

The root creates separate platform and execution VPCs, VPC peering, an application
private DNS zone associated with both, an internal ALB, private tasks/RDS, AWS
endpoints, a two-replica CONNECT proxy and a fail-closed Resolver firewall.
Supply existing company VPC IDs in `additional_application_vpc_ids` to associate
them with the application zone in this region. This composition owns all of the
zone's inline VPC associations; do not manage additional associations separately.
Alternatively, configure company DNS forwarding to the VPC resolver.
DNS association enables name resolution and grants no network ingress.
Company connectivity, DNS forwarding and ClickHouse routing are not provisioned.
Use the emitted VPC, private route table, zone and task security group IDs to
complete those existing network relationships. ClickHouse's owner must allow
only the emitted task group at `clickhouse_port`. Do not attach extra permissive
security groups to tasks.

Private services in the execution VPC can consume `execution_private_subnet_ids`
and `execution_private_route_table_id` directly. These outputs depend on the
execution network, so service or Worker activation does not require rediscovering
that network through data sources that depend on the whole composition.

Copy `terraform.tfvars.example`, replace placeholders and explicitly choose all
Environment limits. Keep `create_controlplane_service=false` and
`create_worker=false` until prerequisites, secret population, database bootstrap,
telemetry bootstrap and migrations are complete. An apply is a real infrastructure
and spending operation; review the saved plan and use your normal approval process.

## Paths and controls

| Caller | Destination and enforcement |
| --- | --- |
| Company browser/CLI | Canonical `https://app_hostname` through existing company connectivity; ALB HTTPS allows only declared company CIDRs and named platform/worker groups |
| Worker host | Same private HTTPS origin over VPC peering; host enrollment/heartbeat authority is unchanged |
| Control Plane/Dispatcher | PostgreSQL TCP 5432, declared private ClickHouse HTTPS port, internal ALB TCP 443, proxy TCP 3128, regional S3 prefix list TCP 443 and AWS interface endpoints TCP 443 |
| ECS execution | ECR API/DKR, S3 layer bucket, Secrets Manager and CloudWatch Logs endpoints; task/execution IAM remains resource-scoped |
| Managed wrapping | KMS endpoint only when `deployment_mode=managed-cloud`; self-hosted wrapping is local |
| Proxy | HTTPS CONNECT to exact GitHub domains, enabled Slack domains, and Resend when selected; rejects other methods/ports, numeric authorities and private/metadata addresses |

Tasks have no general internet egress rule or public IP. The proxy alone has
external HTTPS egress. NAT remains for the proxy and for excluded worker/build
paths. IPv6 is not configured. DNS requests to the VPC resolver do not obey
security groups; the Resolver firewall permits configured services and blocks
other names with fail-open disabled. Approved names may resolve through provider
CNAME chains within that query; directly querying an unlisted target is still
blocked. This follows [AWS DNS redirection semantics](https://docs.aws.amazon.com/Route53/latest/DeveloperGuide/resolver-dns-firewall-rule-settings.html).
Custom resolvers are not allowed by task SGs.
The S3 gateway policy restricts objects to the CAS/Platform stores plus read-only
ECR layer downloads; IAM remains required.

`OUTBOUND_PROXY_URL` applies to GitHub, Slack and Resend clients. S3/KMS,
PostgreSQL, configured ClickHouse and internal readiness calls use direct service
paths. No direct fallback or ambient `HTTP_PROXY` bypass is used. This reference
supports no email or Resend; a private SMTP path needs separate operator design.

The application uses one `APP_ORIGIN`; it has no private-mode branch. Standard selects HTTP Slack by default; Quickstart defaults to Socket. Private permits Socket or
disabled. Socket setup needs an app-level token with `connections:write` from the
same dedicated Slack app. Its hello app ID must match the OAuth-bound app before
any receipt. Each Dispatcher owns at most 32 apps, with one live PostgreSQL lease
per app across replicas. Add Dispatcher replicas when more apps require sockets.
Credentials rotate without logging token values or temporary WSS URLs. The
Console distinguishes installation authorization from observed socket health.

## Guest boundary and limits

`worker_network_blocked_ipv4_cidrs` must cover the control-plane and execution
VPCs in every current and retained Worker generation. Routed company prefixes
outside RFC1918 also require absolute coverage. RFC1918 company routes grant no
connectivity by themselves: private destinations are denied except within
`worker_network_private_business_cidrs` (default `[]`). Business ranges must not
overlap management denies. The same installation inputs configure control-plane
publication checks and authenticated Worker admission and renewal.

Changing these inputs requires an explicit drained rollout. Make old generations
ineligible for every new allocation, settle in-flight admissions, then verify
forwarding, proxy connections and conntrack teardown before activating changed
restrictions. A stricter Worker may reduce reachability during rollout; its
business ceiling must be a subset of the control-plane ceiling and its absolute
denies must cover the control-plane denies. No configuration change terminates
already established traffic by itself. Routing and CA trust remain operator work.
Follow the [full-stop maintenance procedure](../../../packages/web/src/content/docs/self-hosting/upgrades.md)
when replacing Worker generations, including fresh Host readiness and
claim-fenced primary-pool selection before admitting new work. Creating a new
generation does not switch an existing Worker Group's primary pool.
A source security group alone cannot distinguish a translated guest packet from
its host. Qualification must test host success and guest denial together.

Authored bundles upload through the private Helmr HTTPS origin. The Control Plane
checks current permissions and admitted object descriptors, then streams bytes to
S3 through its gateway endpoint. A bucket policy denies `PutObject` to
`quarantine/*` from every other path, including otherwise valid signed requests.
There is no direct-client S3 upload or automatic transport fallback. Quarantine
has a one-day current/noncurrent version lifecycle; published CAS and checkpoints
are outside that prefix. S3 expiration is asynchronous, so it is not an exact
24-hour deletion guarantee.

Worker checkpoint publication and restore use the execution VPC's platform storage
path. Guest downloads follow `prepareNetwork` or `network`, intersected with the
installation denies and business-network ceiling described above. Qualify those
boundaries during preparation, execution, wake and replacement with allowed
business and host-reachability controls. Normal private storage routing does not
prove that outside requests to published CAS/checkpoints are denied: this bucket
restriction covers quarantine writes only. Image pulls and host/CLI builds have
separate operator-controlled network paths.

This deployment does not provide an air gap: Slack messages arrive through an
established outbound socket, and users' browsers contact Slack/GitHub. Allowed
integrations can carry application data.

An external scaler must reach the private capacity API with its own credential.
Use demand for scale-out and drain plus the exact current `termination_ready`
claim before scale-in. Instance selection, budget and provider actions remain
operator responsibilities; changing a network recipe does not change KVM,
Firecracker, snapshot or storage compatibility requirements.

## Qualification

Local source gates are `nix run .#ci-infra-test`, Product Go/PostgreSQL and Console
checks, and `python3 infra/aws/private/tests/test_proxy.py` with Docker available.
The proxy test uses test-owned local TLS endpoints, the pinned image and no Slack
credentials. These checks do not prove AWS routing or a real Slack installation.

Before declaring a deployment ready, preserve exact image/source/configuration
identities and verify:

1. From a company-connected client, HTTPS/CLI/Console/streaming work at the private
   origin. From an external client, application listeners are unreachable.
   Verify deployment uploads use the authenticated private origin and direct
   quarantine writes from outside the approved S3 endpoint are denied.
2. GitHub login, Slack app installation and user linking return through the private
   browser callback. No public callback relay or temporary public listener exists.
3. Real Slack messages, choices, modal opening/submission and duplicate delivery
   work. Interrupt a socket/Dispatcher, rotate credentials and revoke access;
   verify recovery, durable receipt-before-ACK and current authority.
4. API/Dispatcher startup, ECR pulls, secrets, logs, database, ClickHouse and S3 IO
   work under the actual rules. Deny an integration destination and stop the
   proxy: its client must fail without directly accessing the internet.
5. Arbitrary HTTPS, alternate ports, direct IPs, unapproved DNS and metadata/private
   targets through the proxy fail. Record enabled destination policy and DNS results.
6. Worker-host registration, heartbeat and replacement succeed while a guest on
   that same host cannot reach any management/company prefix.
7. Run the same Product image in the public standard recipe with HTTP delivery,
   and retain quickstart regression results.
8. Destroy temporary resources through native state and verify provider absence;
   record any scheduled KMS/secret deletion windows or retained artifacts.

Local mock and Docker tests are not a substitute for this real-network evidence.

Diagnostic ingestion and export limits must be explicitly supplied through
`controlplane_environment` and `dispatcher_environment`; see the shared
[diagnostic process limits](../README.md#diagnostic-process-limits). The example
values are development starting points and require operator review.


Upload capacities and deadlines are explicit common Product settings; see
[deployment upload capacity](../README.md#deployment-upload-capacity) before
choosing values for the selected task size and ingress.
