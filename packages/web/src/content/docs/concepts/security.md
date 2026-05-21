---
title: Security
description: The security boundaries and data handling rules in Helmr.
section: Concepts
sidebarLabel: Security
order: 190
---

# Security

Helmr is designed around explicit runtime boundaries: your control plane, your workers, your GitHub App, declared secrets, scoped permissions, and isolated Linux guests.

## Isolation

Workers execute task code in Firecracker-backed Linux guests. A run receives a GitHub checkout, deployment task source, task-declared secrets, and a bounded duration. Worker capabilities include runtime architecture, kernel and rootfs digests, CNI profile, vCPU, memory, and execution slots.

## Credentials

Secrets are stored encrypted and scoped to a project environment. API keys are stored by hash, can expire or be revoked, and carry explicit project and environment grants.

Supported API key permissions are `runs:create`, `runs:read`, `waitpoints:respond`, `secrets:use`, `secrets:write`, and `tasks:deploy`.

## Payloads Are Plaintext

Run payload is audit data. Helmr persists it in plaintext in the database, run events, and event streams. Do not put tokens, API keys, credentials, or sensitive personal data in payloads.

## GitHub Workspaces

Runs can only check out GitHub repositories that the Helmr GitHub App can access and that are enabled for the project. The control plane resolves refs before workers execute the run.

## Checkpoint Encryption

Checkpoint artifacts are encrypted before leaving the worker staging directory. Workers require `HELMR_CHECKPOINT_ENCRYPTION_KEY`, a base64-encoded 32-byte key, and workers that must restore the same checkpoint state need the same key.
