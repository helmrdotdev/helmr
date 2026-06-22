---
title: Task sessions
description: Task invocation history, channels, runs, and workspace attachment.
section: Concepts
sidebarLabel: Task sessions
order: 156
---

# Task Sessions

A task session is one task invocation and its durable interaction history. It
records the selected task, session status, channel records, related runs, and
the workspace the task uses.

A task session does not own the workspace. It references a workspace.

## What A Session Owns

Task sessions own task-specific history:

- session status
- channel input and output records
- the ordered runs for this task invocation
- task output and failure state
- wait behavior tied to the task invocation

Use sessions when you need to answer "what happened during this task
conversation or workflow?"

## What The Workspace Owns

The workspace owns filesystem and live workspace state:

- durable workspace identity
- sandbox compatibility
- live materialization state
- direct exec handles
- PTY sessions
- stream cursors for direct workspace operations

Use workspaces when you need to answer "what work state exists now?" or "what
can I run against this workspace next?"

## Channels

Session channels are named durable input and output lanes. They are useful for
follow-up messages, webhook replies, operator responses, and structured task
output that belongs to the task invocation.

Channel input and output are session-scoped. Direct workspace exec and PTY
streams are workspace-scoped and use their own APIs.
