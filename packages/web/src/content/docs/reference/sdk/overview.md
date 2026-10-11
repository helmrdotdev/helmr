---
title: TypeScript SDK
description: Agent authoring and explicitly authenticated resource clients.
---

# TypeScript SDK

Use `@helmr/sdk` to declare Agents, Computers and images, configure the build,
and create an authenticated `HelmrClient`.

```ts
import { agent, computer, image, triggers, defineConfig, HelmrClient } from "@helmr/sdk"
```

Agent handlers receive injected Turn operations for output, responses, questions,
steering and Session creation. These carry current runtime authority. An explicit
client always acts under its supplied credentials, even inside Agent code.

Definition IDs match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. Business input, machine
results and ask answers are JSON. Human-facing input, output, responses and
question prompts use Content. Output and responses also accept string shorthand. Await asynchronous
operations and join work before returning from a Turn.

- [Agents, Sessions and Turns](/docs/reference/sdk/agents-and-sessions)
- [Computers](/docs/reference/sdk/computers)
- [Questions](/docs/reference/sdk/questions)
- [Schedules](/docs/reference/sdk/schedules)
- [Authenticated client](/docs/reference/sdk/helmr-client)

Use ordinary language facilities for short delays and process diagnostics. Public
Session events contain authored content and lifecycle facts; process stdout and
stderr are not a public Session log API.
