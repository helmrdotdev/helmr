---
title: Private deployments
description: Keep Helmr application ingress on your company network while permitting specific platform integrations.
sidebarLabel: Private deployment
---

# Private deployments

The `infra/aws/private` reference uses the same Product image as public
quickstart/standard. Console, CLI and API use an internal HTTPS load balancer and
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

This is private application ingress, not an air gap. Slack still sends inputs
through the outbound connection. Existing short-lived presigned S3 uploads are
client-direct and may use public S3 endpoints. Guest/template/model/build egress
is outside this reference's guarantee. Keep the existing guest management-network
denies in place before adding private routes, and verify host access and guest
denial together.

External capacity automation must reach the authenticated private capacity API.
Drain and wait for the exact current `termination_ready` claim before terminating
hosts; see [custom capacity scaling](/docs/self-hosting/capacity).
