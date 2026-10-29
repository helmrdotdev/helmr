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
bindings. Sessions sharing a Computer are mutually trusted, including files, native
histories, credentials, background processes and reachable relay authority.
Agents with different trust requirements need separate Computers and
caller/delegate placement restrictions; an Environment label or descriptor scope alone does not establish
process isolation. See [Execution policies](/docs/concepts/execution-policies).

The local Linux builder installs dependencies and produces immutable artifacts.
Package lifecycle scripts are untrusted build input. The isolated installation
receives only explicitly selected build Secrets, without a host Docker socket or
unrestricted host filesystem. The Control Plane verifies the completed artifact
closure rather than running project installation. The host-side Helmr config is
trusted local code with the invoking user's authority.

## Network and managed-operation limits

Template `network` fixes the Computer's outbound maximum, with a separately
resolved `prepareNetwork` phase. Agent `runtimeMcp` fixes a Session's optional
managed-operation maximum; a connection getter can narrow it. Omitted network
policy retains ordinary network access. Omitted MCP policy resolves at publication
to the exact Agent set in that Deployment, with new Computer placement. Runtime
operations stay within the caller’s workload relationships; new human operations
and delivery still require live authorization.

These are distinct boundaries. An authorized delegate retains its own policies,
and sharing the caller’s own Computer requires explicit `computer: "caller"`
permission. Runtime discovery does not expose unrelated Computers.
Result/control relays and application data remain channels even with external
network traffic blocked. See [Execution policies](/docs/concepts/execution-policies)
for exact scope, native injection and pinned readback.

## Credentials and questions

Environment API keys receive explicit Role assignments, including Agent targets. An authenticated client
uses those credentials even inside Agent code. Injected Turn operations and a
managed Session-bound MCP connection use their runtime authority instead.

Questions have exact Session, Turn and ask identities. Answering requires current
membership or an explicit API-key Role grant and records the authenticated responder.
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
