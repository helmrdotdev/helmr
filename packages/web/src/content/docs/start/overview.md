---
title: Overview
description: What Helmr is, what it owns, and where to start.
section: Start
sidebarLabel: Overview
order: 10
---

# Overview

Helmr is a self-hosted runtime for coding agents. It gives agent tasks a real GitHub checkout, an isolated Linux filesystem, controlled credentials, logs, run history, and waitpoints before side effects continue.

Task code is TypeScript. It can call any agent SDK or command-line tool; Helmr owns the runtime boundary around it: task deployment, workspace checkout, sandbox execution, secret injection, logs, events, and human approval points.

## What Helmr Provides

- A TypeScript SDK for declaring tasks, images, sandboxes, resources, secrets, and waitpoints.
- A CLI for login, task deployment, runs, logs, events, waitpoint responses, and remote secrets.
- A control plane that stores projects, environments, task deployments, runs, waitpoints, logs, events, secrets, and API keys.
- Workers that lease runs and execute them inside Firecracker-backed Linux guests.

## First Path

Use the quickstart to run the local control plane. Then define a task project, deploy it, and start a GitHub-backed run against a repository that your Helmr GitHub App can access.
