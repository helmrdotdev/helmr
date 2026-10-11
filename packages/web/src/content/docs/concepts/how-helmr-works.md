---
title: How Helmr works
description: The resources Helmr uses to run deployed Agents in durable Linux Computers.
---

# How Helmr works

Helmr runs JavaScript and TypeScript Agents in Linux Computers. You choose the
harness, model, tools and work procedure inside the Agent. Helmr admits work,
runs it one Turn at a time, keeps the Computer's state across Turns and retains
each outcome.

| Resource | What it is |
| --- | --- |
| [Agent](/docs/concepts/agents) | A deployed definition: a Computer recipe, optional `init` and a `turn` handler. |
| [Computer](/docs/concepts/computers) | A Linux working environment with a retained disk and running processes. |
| [Session](/docs/concepts/sessions) | One continuing conversation with an Agent, pinned to a Deployment and a Computer. |
| [Turn](/docs/concepts/turns) | One admitted input and its terminal outcome. Turns in a Session run one at a time. |
| [Project and Deployment](/docs/concepts/projects) | A Project holds Environments; a Deployment is an immutable build promoted to be current in one Environment. |

## From code to a running Session

You define Agents and Computers in TypeScript, build them into a Deployment
with the CLI and promote that Deployment in an Environment. Starting an Agent
creates a Session and its first Turn. The Session gets a Computer, runs the
Agent's `init` once and then calls `turn` for each admitted input in order.
Later inputs join the same Session. See [Agents](/docs/concepts/agents) and
[Sessions](/docs/concepts/sessions).

A Turn can write output, ask a person a [question](/docs/concepts/questions)
and return a JSON result. A Turn is `completed` only after the Computer's disk
is saved durably. Between Turns, an idle Session can hibernate and resume
without rerunning `init`. See [Turns](/docs/concepts/turns) and
[Durability](/docs/concepts/durability).

Work can also start from a [cron trigger](/docs/concepts/triggers), from
[Slack](/docs/concepts/slack) or from another Agent through
[delegation](/docs/concepts/delegation).

## Identity and retries

Operations address resources by ID. A Session key names a continuing
conversation so a later start reaches the same Session. An idempotency key
identifies one request so an uncertain retry does not admit the work twice.

## Data and credentials

Keep credentials in [Secrets](/docs/concepts/secrets). Inputs, results,
output, questions, answers and event history are retained application data and
must not contain credentials. See [Security](/docs/concepts/security).
