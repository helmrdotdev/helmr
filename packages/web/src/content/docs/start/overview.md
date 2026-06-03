---
title: Overview
description: What Helmr is, what it owns, and where to start.
section: Start
sidebarLabel: Overview
order: 10
---

# Overview

Helmr is a self-hosted runtime for coding agents. It gives agent tasks an isolated writable workspace, controlled credentials, logs, run history, and waitpoints before side effects continue.

Task code is TypeScript. It can call any agent SDK or command-line tool; Helmr owns the runtime boundary around it: deployment, workspace setup, sandbox execution, secret injection, logs, events, and human approval points.

## What Helmr Provides

- A TypeScript SDK for declaring tasks, images, sandboxes, resources, secrets, and waitpoints.
- A CLI for login, deployments, runs, logs, events, waitpoint responses, and remote secrets.
- A control plane that stores projects, environments, deployments, runs, waitpoints, logs, events, secrets, and API keys.
- Workers that lease runs and execute them inside Firecracker-backed Linux guests.

## First Path

Use the quickstart to run the local control plane. Then define a task project, deploy it, and start a run with an empty writable workspace.
