---
title: Overview
description: What Helmr is, what it owns, and where to start.
section: Start
sidebarLabel: Overview
order: 10
---

# Overview

Helmr is a self-hosted runtime for coding agents. It gives Tasks and Actors
durable Workspaces, isolated execution, controlled credentials, logs, durable
Actor input/output, Run history, and waits before side effects continue.

Task code is TypeScript. It can call any agent SDK or command-line tool; Helmr
owns the runtime boundary around it: deployment, workspace lifecycle, sandbox
execution, secret injection, logs, events, and operator approval points.

## What Helmr Provides

- A TypeScript SDK for declaring Tasks, Actors, Workspaces, source Schedules,
  Secrets, waits, Tokens, metadata, and logs.
- A runtime client for starting Tasks, operating Workspaces, and inspecting
  Runs.
- A CLI for login, Deployments, Task starts, Run inspection,
  bounded Workspace exec and remote Secrets.
- A control plane that stores Projects, Environments, Deployments, Workspaces,
  Actors, Runs, waits, Actor records, metadata, logs, events, Secrets,
  and API keys.
- Workers that materialize workspaces, lease runs, and execute them inside
  Firecracker-backed Linux guests.

## First Path

Use the quickstart to run the local control plane. Then define a task project,
deploy it, and start a task that creates or attaches to a durable workspace.
