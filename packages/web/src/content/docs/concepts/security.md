---
title: Security
description: Environment authority, Computer isolation and attributable interaction.
---

# Security

Helmr separates Environment authority, Computer execution and retained
application data. Native harnesses retain their own permission policies.

## Runtime and build boundaries

Agents and Computer Commands execute inside Linux guests on workers. Computer
definitions select the image and resources; actual Computers have fixed Secret
bindings. Sessions sharing a Computer share files and process authority. Agent code
within an Environment is mutually trusted, including reachable files, native
histories, credentials and services. Helmr does not promise isolation for mutually
untrusted code; an Environment label alone does not establish that boundary.

The local Linux builder installs dependencies and produces immutable artifacts.
Package lifecycle scripts are untrusted build input. The isolated installation
receives only explicitly selected build Secrets, without a host Docker socket or
unrestricted host filesystem. The Control Plane verifies the completed artifact
closure rather than running project installation. The host-side Helmr config is
trusted local code with the invoking user's authority.

## Credentials and questions

Environment API keys carry explicit operation permissions. An authenticated client
uses those credentials even inside Agent code. Injected Turn operations and a
managed Session-bound MCP connection use their runtime authority instead.

Questions have exact Session, Turn and ask identities. Answering requires current
membership or explicit API-key permission and records the authenticated responder.
Channel visibility alone does not grant that authority. Native permission adapters
must validate the live callback and propagate cancellation; a retained answer or
output receipt does not grant permission to a different request.

## Data and Secret handling

Inputs, results, authored content, question answers, event history and retained
Computer files are durable data surfaces. Keep credentials out of them. Secret
reads return metadata; values are delivered only through selected bindings.
Protected env substitutes credentials at approved HTTPS origins, while raw env
and files expose values to Computer processes. See [Secrets](/docs/concepts/secrets)
for exact boundaries and unsupported transports.

Use stable, nonsensitive request identities for idempotency. They reconcile
uncertain delivery but do not authenticate callers or make external effects
exactly once.

## Retention and stopping

A completed Turn does not delete its Session or Computer. Controls acknowledge
intent separately from physical convergence. Explicit Computer deletion can remain
busy while associated Sessions, execution or saving remain unresolved. Plan
retention and cleanup around the resources that hold the data.
