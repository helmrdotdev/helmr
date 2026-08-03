---
title: Security
description: The security boundaries and data handling rules in Helmr.
section: Concepts
sidebarLabel: Security
order: 190
---

# Security

Helmr is designed around explicit runtime boundaries: your Control Plane, your workers, declared secrets, scoped permissions, and isolated Linux guests.

## Isolation

Workers execute task code and direct workspace operations in
Firecracker-backed Linux guests. A run receives an attached writable workspace,
deployment task source, task-declared secrets, and a bounded duration. Worker
capabilities include runtime architecture, kernel and rootfs digests, network
ABI, vCPU, memory, and execution slots.

## Credentials

Secrets are stored encrypted and scoped to a project environment. API keys are stored by hash, can expire or be revoked, and are bound to one project environment. API key grants describe allowed actions inside that environment.

API-key capabilities are explicit and Environment-scoped. Actor input and
output use `actors.input.send` and `actors.read`; named Session-stream
permissions do not exist. Token, Secret, Run, Workspace, and deployment
permissions remain separate. Secret API responses never return Secret values.

## Payloads Are Plaintext

Run payload is audit data. Helmr persists it in plaintext in the database and
telemetry. Do not put tokens, API keys, credentials, or sensitive personal data
in payloads.

## Workspaces

Workspaces are durable project-environment objects. A task run attaches to a
workspace; if none is supplied, Helmr creates one from the task's deployed
sandbox. If a task needs a repository or external data, pass the reference in
payload and a scoped credential as a task secret so the task can fetch it inside
the guest.

## Checkpoint Encryption

Checkpoint artifacts are encrypted before leaving the worker staging directory. Workers require `CHECKPOINT_ENCRYPTION_KEY`, a base64-encoded 32-byte key, and workers that must restore the same checkpoint state need the same key.
