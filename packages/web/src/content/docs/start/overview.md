---
title: Overview
description: What Helmr is, what it owns, and where to start.
section: Start
sidebarLabel: Overview
order: 10
---

# Overview

Helmr is a self-hosted runtime for coding agents. It gives agent tasks durable
workspaces, isolated execution, controlled credentials, logs, session channels,
run history, and waitpoints before side effects continue.

Task code is TypeScript. It can call any agent SDK or command-line tool; Helmr
owns the runtime boundary around it: deployment, workspace lifecycle, sandbox
execution, secret injection, logs, events, and operator approval points.

## What Helmr Provides

- A TypeScript SDK for declaring tasks, images, sandboxes, resources, secrets,
  session channels, metadata, waits, waitpoint tokens, and logs.
- A runtime client for starting tasks, opening workspaces, creating execs, and
  opening PTY sessions.
- A CLI for login, deployments, task starts, session I/O, run inspection,
  workspace exec and PTY, waitpoint responses, and remote secrets.
- A control plane that stores projects, environments, deployments, workspaces,
  sessions, runs, waitpoints, channel records, metadata, logs, events, secrets,
  and API keys.
- Workers that materialize workspaces, lease runs, and execute them inside
  Firecracker-backed Linux guests.

## First Path

Use the quickstart to run the local control plane. Then define a task project,
deploy it, and start a task that creates or attaches to a durable workspace.
