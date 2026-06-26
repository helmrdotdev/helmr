---
title: Sessions
description: Task invocation history, streams, runs, and workspace attachment.
section: Concepts
sidebarLabel: Sessions
order: 156
---

# Sessions

A session is one task invocation and its durable interaction history. It
records the selected task, lifecycle status, derived activity, stream records,
related runs, and the workspace the task uses.

A session does not own the workspace. It references a workspace.

## What A Session Owns

Sessions own task-specific history:

- lifecycle status: `open`, `closed`, `cancelled`, or `expired`
- derived activity: `idle`, `queued`, `running`, or `waiting`
- stream input and output records
- the ordered runs for this task invocation
- task output and failure state
- wait behavior tied to the task invocation

Session status describes the lifecycle of the conversation or workflow. Run
success and failure live on runs, not on the session status. The session
activity field is derived from the current run, wait state, and pending
continuation input.

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

## Streams

Session streams are named durable input and output lanes. They are useful for
follow-up messages, webhook replies, operator responses, and structured task
output that belongs to the task invocation.

Streams are not project-level resources. A stream name only has meaning
inside its session.

Use input streams when something outside the task should continue the workflow,
such as a product UI, webhook handler, or approval bridge. Use output streams
when the task should publish structured records that another service can read or
stream without parsing logs.

Stream input and output are session-scoped. Direct workspace exec and PTY
streams are workspace-scoped and use their own APIs.
