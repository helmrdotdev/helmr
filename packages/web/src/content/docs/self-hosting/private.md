---
title: Private deployments
description: Keep Helmr application ingress on your company network while permitting specific platform integrations.
sidebarLabel: Private deployment
---

# Private deployments

The `infra/aws/private` reference uses the same Product image as public
standard. Console, CLI and API use an internal HTTPS load balancer and
a canonical `APP_ORIGIN` reachable from your company network. Your platform team
owns private connectivity, DNS/TLS, service endpoints, egress and capacity policy.

Slack uses outbound Socket Mode and needs no public event or interaction URL.
Organization owners/admins still create a dedicated app per Agent and save its
credentials in Console. Users must reach both Slack/GitHub and the private
Console from their browsers for installation, login and identity linking. See
[Connect an Agent to Slack](/docs/guides/connect-slack).

The reference restricts platform task egress to a maintained CONNECT proxy,
PostgreSQL, a private ClickHouse endpoint, internal HTTPS and named AWS
endpoints. Its resolver policy restricts DNS separately. Proxy failure does not
select a direct internet path. Public standard deployments keep their existing
normal external access; the private controls are a separate operator recipe.

Read `infra/aws/private/README.md` for required inputs, startup dependencies and
the real-network acceptance procedure. Its local tests check source composition
and proxy/Socket behavior. Qualify the exact deployment in your own network before
using it for customer work.

Bundle uploads use the private Helmr origin, with current permissions and content
checks on each upload. The Control Plane writes through its S3 gateway endpoint;
the bucket denies quarantine writes from other paths. Clients need no separate
S3 route. Configure and measure the common [upload capacity and timeouts](/docs/reference/env#control-plane) for the selected server size and ingress. Published object and checkpoint access retain their platform storage
policies; the quarantine rule does not restrict the entire bucket.

Guest downloads follow the declared preparation/runtime network policy and the
installation's mandatory restrictions. Verify allowed business access, host access
and guest management-network denial during preparation, execution, wake and host
replacement. Worker checkpoint publication and restore use platform paths.
Image pulls and host/CLI builds have their own operator-controlled paths.

This is not an air gap. Slack sends inputs through its outbound connection, and
allowed integrations can carry application data.

External capacity automation must reach the authenticated private capacity API.
Drain and wait for the exact current `termination_ready` claim before terminating
hosts; see [custom capacity scaling](/docs/self-hosting/capacity).
