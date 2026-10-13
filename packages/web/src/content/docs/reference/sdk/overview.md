---
title: TypeScript SDK
description: Exports of the @helmr/sdk package and an index of the SDK reference pages.
sidebarLabel: Overview
---

# TypeScript SDK

`@helmr/sdk` declares Agents, Computers, images and triggers, configures the
build, and provides an explicitly authenticated `HelmrClient`. Agent
context exposes `ctx.mcp.getConnectionOptions()` for the runtime MCP tools.

```ts
import { agent, template, image, triggers, defineConfig, HelmrClient } from "@helmr/sdk"
```

## Reference pages

| Page | Covers |
| --- | --- |
| [Agent](/docs/reference/sdk/agent) | `agent()`, `AgentDefinition`, `Turn`, `InitContext`/`AgentContext`, `RuntimeMcpConnectionOptions`, `MessageRejected`, and the client's Session and Turn methods. |
| [Computer](/docs/reference/sdk/computer) | `template()`, `image()`, `source`, Secret bindings, and the client's Computer, Template definition and Command methods. |
| [Client](/docs/reference/sdk/client) | `HelmrClient` options and properties, Agent and definition reads, Deployments and Secrets. |
| [Triggers](/docs/reference/sdk/triggers) | `triggers.cron()` and the client's read-only Schedule methods. |
| [Questions](/docs/reference/sdk/questions) | `turn.ask()`, answer controls, answer shapes and limits, and the client's ask methods. |
| [Runtime MCP](/docs/reference/sdk/mcp) | `ctx.mcp.getConnectionOptions()` and every runtime MCP tool, with inputs and results. |

Build configuration (`defineConfig`, `builder`) is documented in
[Configuration](/docs/reference/config).

## Exports

Values exported from `@helmr/sdk`:

| Export | Kind | Reference |
| --- | --- | --- |
| `agent` | function | [Agent](/docs/reference/sdk/agent) |
| `computer` | function | [Computer](/docs/reference/sdk/computer) |
| `triggers` | object with `cron()` | [Triggers](/docs/reference/sdk/triggers) |
| `image`, `source` | function, object | [Computer](/docs/reference/sdk/computer#images) |
| `builder` | function | [Configuration](/docs/reference/config#build-environment) |
| `defineConfig` | function | [Configuration](/docs/reference/config) |
| `HelmrClient` | class | [Client](/docs/reference/sdk/client) |
| `MessageRejected` | error class | [Agent](/docs/reference/sdk/agent#messagerejected) |

Types exported from `@helmr/sdk`, grouped by area:

| Area | Types |
| --- | --- |
| Definitions | `AgentDefinition`, `TemplateDefinition`, `CronTrigger`, `ComputerIdentity`, `BuildContext`, `SecretBinding`, `Duration`, `Json`, `JsonValue`, `MaybePromise` |
| Turn handler | `Turn`, `InitContext`, `AgentContext`, `RuntimeMcpConnectionOptions`, `InputSource`, `InputMessage`, `OutputReceipt`, `TurnOutcome` |
| Content | `ContentPart`, `Content`, `HumanContent`, `InputPart`, `InputContent` |
| Questions | `ChoiceOption`, `AnswerControl`, `Question`, `ChoiceAnswer`, `AnswerFor`, `AskResponse`, `AskState`, `AskResponseReceipt`, `AskQuery`, `AskPage`, `TurnAsks` |
| Client | `HelmrClientOptions`, `RequestOptions`, `CursorPage`, `HelmrError`, `APIError` |
| Agents | `ClientAgentsApi`, `SessionCreateRequest`, `SessionCreateReceipt`, `ClientAgentsApi`, `AgentGetQuery`, `AgentListQuery`, `AgentListItem`, `AgentInfo`, `AgentPage` |
| Sessions and Turns | `ClientSessionsApi`, `ClientSessionRef`, `SessionListQuery`, `SessionRef`, `Session`, `SessionStatus`, `SessionHold`, `SessionOperationOptions`, `SessionCreateReceipt`, `SessionMessageReceipt`, `SessionCloseReceipt`, `SessionCancelReceipt`, `SessionInterruptReceipt`, `SessionResumeRequest`, `SessionResumeReceipt`, `SessionEvent`, `SessionEventKind`, `SessionEventPage`, `SessionEventQuery`, `MessageReceipt`, `TurnRef`, `TurnState`, `TurnStatus`, `TurnListQuery`, `TurnWaitOptions`, `TimedTurnWaitResult` |
| Computers | `ClientComputersApi`, `ClientComputerRef`, `ComputerRef`, `Computer`, `ComputerStatus`, `ComputerResidency`, `ComputerListItem`, `ComputerListQuery`, `ComputerCreateRequest`, `ComputerDeleteRequest`, `ComputerDeleteReceipt`, `ComputerCommandRequest`, `ComputerUsageQuery`, `ComputerUsage`, `ClientTemplatesApi`, `TemplateInfo`, `TemplateListItem`, `TemplateListQuery`, `TemplatePage`, `TemplateGetQuery`, `ImageBuilder`, `SourceDirectory`, `SourceFile` |
| Commands | `CommandRef`, `CommandInfo`, `CommandOutcome`, `CommandWaitOptions`, `TimedCommandWaitResult`, `CancelReceipt`, `CommandLogQuery`, `CommandLogStreamQuery`, `CommandLogRecord`, `CommandLogReadPage` |
| Deployments | `ClientDeploymentsApi`, `Deployment`, `DeploymentListItem`, `DeploymentListQuery` |
| Schedules | `ClientSchedulesApi`, `Schedule`, `ScheduleListQuery` |
| Secrets | `ClientSecretsApi`, `SecretRef`, `Secret`, `SecretStatus`, `SecretCreateRequest`, `SecretListQuery`, `SecretRotateRequest`, `SecretRevokeRequest` |
| Configuration | `HelmrConfig`, `HelmrConfigInput`, `HelmrBuildConfig`, `HelmrBuildInput`, `Builder`, `BuilderStep` |

## Shared conventions

- Definition IDs match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`.
- `Duration` is a positive safe integer number of milliseconds, or a string of
  a positive integer followed by `ms`, `s`, `m`, `h` or `d` (for example
  `"30s"`, `"7d"`). Compound and fractional strings are rejected. Retention
  options (`closeAfterIdle`, `deleteAfterUnused`) also accept `"never"`.
- Input is always an array of text parts (`InputContent`); omit initial input to create a Session with zero Turns; explicit empty initial input is rejected.
  Output, responses and question prompts use `Content`, which also allows JSON
  parts. Machine results and answers are JSON.
- Client methods accept an optional final `RequestOptions` argument,
  `{ signal?: AbortSignal }`.
- Client list methods return `CursorPage<T>`: `{ items, nextCursor? }`. Treat
  cursors as opaque. See [Pagination](/docs/reference/api/pagination).
- Operations inside a Turn handler (`turn.output`, `turn.respond`, `turn.ask`,
  `turn.onMessage`) and runtime MCP tools act with the current Session's runtime
  authority. A `HelmrClient` always acts with its own API key, including when it
  is constructed inside Agent code.
