import type { HelmrClient, WaitpointResponseToken } from "./client"
import type { PendingApprovalWaitpoint, PendingMessageWaitpoint, RunHandle, RunSnapshot } from "./run"
import type { Task } from "../internal"
import { source, workspace } from "../index"

declare const client: HelmrClient
declare const handle: RunHandle
declare const snapshot: RunSnapshot
declare const pendingApproval: PendingApprovalWaitpoint
declare const pendingMessage: PendingMessageWaitpoint
declare const triggerTask: Task<{ issue: number }, {}>
declare const signal: AbortSignal

if (false) {
  workspace.github("helmrdotdev/helmr", { ref: "main" })
  workspace.github("helmrdotdev/helmr", { ref: "0123456789abcdef0123456789abcdef01234567", subpath: "sdk/typescript" })

  const triggered: Promise<RunHandle> = client.tasks.trigger(triggerTask, {
    payload: { issue: 123 },
    workspace: workspace.github("helmrdotdev/helmr", { ref: "main" }),
  })
  const retrievedFromHandle: Promise<RunSnapshot> = client.runs.retrieve(handle)
  const retrievedFromId: Promise<RunSnapshot> = client.runs.retrieve("run-1")
  const waitedFromHandle: Promise<RunSnapshot> = client.runs.wait(handle, {
    timeoutMs: 30_000,
    intervalMs: 250,
    signal,
  })
  const waitedFromId: Promise<RunSnapshot> = client.runs.wait("run-1")
  client.runs.logs.retrieve(handle)
  client.runs.logs.retrieve("run-1")
  client.runs.events.list(handle)
  client.runs.events.list(handle, { pageSize: 50 })
  client.runs.events.list("run-1")
  client.runs.events.subscribe(handle)
  client.runs.events.subscribe("run-1")
  client.runs.list({ status: "running" })
  client.waitpoints.approve(pendingApproval, { reason: "ok" })
  client.waitpoints.approve("run-1", "waitpoint-1", { reason: "ok" })
  client.waitpoints.deny(pendingApproval, { reason: "no" })
  client.waitpoints.reply(pendingMessage, { text: "ok" })
  client.waitpoints.reply("run-1", "waitpoint-1", { text: "ok" })
  const delegatedApproval: Promise<WaitpointResponseToken> = client.waitpoints.tokens.create(pendingApproval, {
    actions: ["approve", "deny"],
    expiresInSeconds: 3600,
    metadata: { recipient: "reviewer@example.com" },
  })
  const delegatedMessage = client.waitpoints.tokens.create(
    { runId: "run-1", waitpointId: "waitpoint-1" },
    { actions: ["message"], expiresAt: "2026-04-20T00:00:00Z" },
  )
  client.waitpoints.tokens.create("run-1", "waitpoint-1", { actions: ["reply"] })
  client.waitpoints.tokens.complete({
    id: "token-1",
    runId: "run-1",
    waitpointId: "waitpoint-1",
    url: "https://api.example.test/waitpoints/respond?id=token-1&token=raw-token",
    token: "raw-token",
    expiresAt: null,
  }, {
    action: "approve",
    reason: "reviewed",
    externalSubject: "alice@example.com",
    metadata: { source: "email" },
  })
  client.waitpoints.tokens.complete("token-1", "raw-token", { action: "deny", reason: "blocked" })
  client.waitpoints.tokens.complete("token-1", "raw-token", { action: "message", text: "ship it" })
  client.waitpoints.tokens.complete("token-1", "raw-token", { action: "reply", text: "continue" })
  snapshot.pendingWaitpoint?.kind

  // Keep the declared promises live without executing this block.
  triggered.then
  retrievedFromHandle.then
  retrievedFromId.then
  waitedFromHandle.then
  waitedFromId.then
  delegatedApproval.then
  delegatedMessage.then

  // @ts-expect-error runs.retrieve accepts a run id string or RunHandle only.
  client.runs.retrieve({ taskId: "inspect" })
  // @ts-expect-error runs.retrieve does not accept arbitrary id-only objects.
  client.runs.retrieve({ id: "run-1" })
  // @ts-expect-error runs.wait accepts a run id string or RunHandle only.
  client.runs.wait({ taskId: "inspect" })
  // @ts-expect-error runs.wait does not accept arbitrary id-only objects.
  client.runs.wait({ id: "run-1" })
  // @ts-expect-error logs.retrieve accepts a run id string or RunHandle only.
  client.runs.logs.retrieve({ id: "run-1" })
  // @ts-expect-error events.list accepts a run id string or RunHandle only.
  client.runs.events.list({ id: "run-1" })
  // @ts-expect-error events.subscribe accepts a run id string or RunHandle only.
  client.runs.events.subscribe({ id: "run-1" })
  // @ts-expect-error leased is an internal transient status, not a public run filter.
  client.runs.list({ status: "leased" })
  // @ts-expect-error events.list uses pageSize because it follows every page.
  client.runs.events.list(handle, { limit: 50 })
  // @ts-expect-error approval actions require approval waitpoints.
  client.waitpoints.approve(pendingMessage, { reason: "no" })
  // @ts-expect-error denial actions require approval waitpoints.
  client.waitpoints.deny(pendingMessage, { reason: "no" })
  // @ts-expect-error replies require message waitpoints.
  client.waitpoints.reply(pendingApproval, { text: "no" })
  // @ts-expect-error reply by id requires reply options.
  client.waitpoints.reply("run-1", "waitpoint-1")
  // @ts-expect-error reply by waitpoint requires reply options.
  client.waitpoints.reply(pendingMessage)
  // @ts-expect-error approval options do not accept message text.
  client.waitpoints.approve("run-1", "waitpoint-1", { text: "no" })
  // @ts-expect-error reply options require text.
  client.waitpoints.reply("run-1", "waitpoint-1", { reason: "no" })
  // @ts-expect-error token actions are limited to supported waitpoint responses.
  client.waitpoints.tokens.create(pendingApproval, { actions: ["skip"] })
  // @ts-expect-error token creation accepts expiresInSeconds or expiresAt, not both.
  client.waitpoints.tokens.create(pendingApproval, {
    expiresInSeconds: 3600,
    expiresAt: "2026-04-20T00:00:00Z",
  })
  // @ts-expect-error token completion requires an action.
  client.waitpoints.tokens.complete("token-1", "raw-token", { reason: "ok" })
  // @ts-expect-error message completion requires text.
  client.waitpoints.tokens.complete("token-1", "raw-token", { action: "message" })
  // @ts-expect-error approval completion does not accept message text.
  client.waitpoints.tokens.complete("token-1", "raw-token", { action: "approve", text: "ok" })
  client.tasks.trigger(triggerTask, {
    payload: { issue: 123 },
    // @ts-expect-error trigger uses workspace, not source.
    source: workspace.github("helmrdotdev/helmr", { ref: "main" }),
  })
  // @ts-expect-error source helpers are only for image file/directory inputs.
  source.tar("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
  // @ts-expect-error source helpers use file() or directory().
  source.path(".")
  // @ts-expect-error workspace.github() requires an explicit ref.
  workspace.github("helmrdotdev/helmr")
  // @ts-expect-error workspace.github() no longer exposes installation selection.
  workspace.github("helmrdotdev/helmr", { installation: "123" })
  // @ts-expect-error workspace.github() no longer exposes fetch policy selection.
  workspace.github("helmrdotdev/helmr", { fetchPolicy: "shallow" })
  // @ts-expect-error workspace.github() uses ref instead of rev.
  workspace.github("helmrdotdev/helmr", { rev: "main" })
}
