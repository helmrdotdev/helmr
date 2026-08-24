---
title: Security
description: Isolation, credentials, data handling, and public capability boundaries.
---

# Security

Helmr separates control authority, execution, and application data through
scoped credentials and isolated runtime resources.

## Runtime isolation

Task and Actor code, plus bounded Workspace exec, run in Firecracker-backed
Linux guests on workers. The Sandbox selects the image, CPU, and memory. Helmr
attaches the approved Workspace and materializes its fixed Secret placements.
Application code should rely on normal guest behavior, not host paths, worker
credentials, guest-control protocols, or networking implementation details.

Deployment bundles are produced in Helmr's digest-pinned Linux builder on the
developer or CI machine. Package lifecycle scripts are untrusted build input,
so the builder receives neither runtime Secrets nor host filesystem or Docker
socket access. The Control Plane verifies the completed artifact closure and
never runs package installation or project build commands.

## Credentials and capabilities

Environment API keys are stored by hash, may expire or be revoked, and carry
explicit actions within one Project Environment. Permissions for Task starts,
Actor starts, Session input/read/close, Runs, Workspaces, Tokens, Secrets, and
Deployments are distinct.

Token creation returns a callback URL and public access token for completing
that one Token. Treat them as bearer credentials and prefer this narrow
capability for public approval links. A Session input capability grants access
to a continuing Actor channel and should remain in trusted integrations.

## Data handling

Run payload, metadata, tags, logs, events, Actor input and output, Token
completion results, and committed Workspace files are durable data surfaces.
Do not place API keys, tokens, passwords, private keys, or unnecessary personal
data in them.

Secrets are encrypted, versioned, environment-scoped values. Public responses
do not return plaintext. Bind them during Workspace creation and read them only
from the declared runtime placement. Avoid printing values or passing them to
child processes in visible command lines.

Use idempotency keys derived from stable upstream operations. They protect
retries from duplicating starts or messages, but they are not authentication
credentials and should not contain sensitive data.

## Durable state

Workspaces outlive Runs unless deleted. Session output and input histories are
durable. Cancellation requests may take time to converge. Plan retention and
cleanup around the resources that actually hold application state rather than
assuming a terminal Run removes them.
