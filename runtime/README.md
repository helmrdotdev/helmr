# Runtime

Runtime adapters that execute user task code live here.

This layer sits between the SDK-facing task modules and the guest protocol. It loads tasks, compiles task metadata during parse, runs task bodies, and translates task-side effects such as approvals into the host protocol.

Current contents:
- `typescript/`: TypeScript adapter used by guestd for parse and run requests. Helmr manages the adapter protocol, but image-mode task code runs on the `node` executable provided by the task sandbox image. Bun is only available when a task sandbox image explicitly provides it as a package manager or command-line tool.

Do not put public SDK authoring APIs here. User-facing APIs belong in `sdk/`; runtime code should focus on execution, adaptation, and protocol translation.
