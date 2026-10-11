---
title: How Helmr works
description: Deployed Agents, retained Sessions, serial Turns and durable Computers.
---

# How Helmr works

Helmr runs deployed JavaScript and TypeScript Agents in Linux Computers. You
choose the native harness, model, tools and work procedure. Helmr manages
admission, execution, retained outcomes and the Computer lifecycle.

| Resource | Purpose |
| --- | --- |
| Deployment | Immutable code and definitions selected in a Project Environment. |
| Agent | A Computer recipe, optional setup and a Turn handler. |
| Computer | A working environment with retained disk and execution state. |
| Session | A continuing work context pinned to a Deployment and Computer. |
| Turn | One admitted input and one terminal outcome, processed serially within a Session. |

Starting an Agent admits a Session and its first Turn. Later inputs enter the same
Session with `enqueue`. A Computer definition is a recipe; an actual Computer is
created from it unless the caller selects an existing one. Sessions sharing a
Computer share files but keep separate identities and native histories.

Setup runs before the first Turn. Its live result is available as
`ctx.setupResult`. Healthy hibernation preserves that result and native processes.
Loss of a valid continuation holds the affected Session for explicit recovery;
Helmr does not silently replay interrupted inputs.

A handler return begins finalization. Completed requires drainage and publication
of the Turn's adequate Computer disk Save. Output can be read before completion.
The machine result and optional human response are separate values.

Use resource IDs for operations. Session keys identify conversations; idempotency
keys reconcile retries of the same request. Promotion selects code for new work;
it does not upgrade existing Sessions.

Keep credentials in Secrets. Inputs, results, human content and event history are
retained application data and must not contain credentials.
