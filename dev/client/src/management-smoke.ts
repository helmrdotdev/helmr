import {
  type WorkspaceRef,
  type HelmrClient,
  type JsonValue,
  type RunHandle,
  type Run,
} from "@helmr/sdk"
import type {
  timerSmoke,
  timerSmokeWorkspace,
} from "../../workflows/tasks/smoke/timer"
import type {
  runtimeSmoke,
  runtimeSmokeWorkspace,
} from "../../workflows/tasks/smoke/runtime"
import { assert, assertEqual } from "./assert"

export type ManagementEvidence = Readonly<{
  deploymentId: string
  secretId: string
  completedTokenId: string
  externalTokenRunIds: readonly string[]
  externalTokenWorkspaceIds: readonly string[]
  cancelledRunId: string
}>

export async function runManagementSmoke(
  client: HelmrClient,
  marker: string,
): Promise<ManagementEvidence> {
  const workspaces: Array<Readonly<{ ref: WorkspaceRef; deleteKey: string }>> = []
  let succeeded = false
  try {
    const deployment = await client.deployments.current({
      signal: AbortSignal.timeout(30_000),
    })
    assert(deployment !== null, "current Deployment was not available")
    const retrievedDeployment = await client.deployments.retrieve(deployment.id, {
      signal: AbortSignal.timeout(30_000),
    })
    assertEqual(
      retrievedDeployment.id,
      deployment.id,
      "Deployment retrieve changed the current Deployment ID",
    )
    const secretName = `release-gate-${marker}`.replace(/[^A-Za-z0-9_.-]/g, "-")
    const secret = await client.secrets.create(
      {
        name: secretName,
        value: `initial:${marker}`,
        idempotencyKey: `secret:create:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    const exactSecretPage = await client.secrets.list(
      { name: secretName },
      { signal: AbortSignal.timeout(30_000) },
    )
    assertEqual(exactSecretPage.items.length, 1, "Secret name lookup was not exact")
    assertEqual(exactSecretPage.items[0]!.id, secret.id, "Secret name lookup changed the ID")
    const retrievedSecret = await client.secrets.retrieve(secret.id, {
      signal: AbortSignal.timeout(30_000),
    })
    assertEqual(retrievedSecret.id, secret.id, "Secret retrieval changed the ID")
    const secretRef = client.secrets.ref(secret.id)
    const secretPage = await client.secrets.list(
      { limit: 100 },
      { signal: AbortSignal.timeout(30_000) },
    )
    assert(
      secretPage.items.some((item) => item.id === secret.id),
      "Secret list omitted the created Secret",
    )
    const rotated = await secretRef.rotate(
      {
        value: `rotated:${marker}`,
        idempotencyKey: `secret:rotate:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    assert(rotated.rotatedAt !== undefined, "Secret rotate omitted rotatedAt")
    const revoked = await secretRef.revoke(
      { idempotencyKey: `secret:revoke:${marker}` },
      { signal: AbortSignal.timeout(30_000) },
    )
    assertEqual(revoked.status, "revoked", "Secret was not revoked")

    const completedToken = await client.tokens.create(
      {
        timeout: "10m",
        tags: ["smoke", "external-completion"],
        metadata: { marker },
        idempotencyKey: `token:complete:create:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    const externalTokenRuns: RunHandle<JsonValue>[] = []
    const externalTokenWorkspaces: string[] = []
    for (const suffix of ["fanout-a", "fanout-b"] as const) {
      const tokenWorkspace = await client.sandboxes.createWorkspace(
        "helmr-runtime-smoke",
        {
          key: `${suffix}-${marker}`,
          idempotencyKey: `token-workspace:create:${suffix}:${marker}`,
        },
        { signal: AbortSignal.timeout(10 * 60_000) },
      )
      workspaces.push({
        ref: tokenWorkspace,
        deleteKey: `token-workspace:delete:${suffix}:${marker}`,
      })
      externalTokenWorkspaces.push(tokenWorkspace.id)
      const run = await client.tasks.start<typeof runtimeSmoke>(
        "runtime-smoke",
        {
          payload: {
            scenario: suffix,
            marker: `${marker}-${suffix}`,
            expectedEnvironment: "unknown",
            exerciseToken: true,
            externalTokenId: completedToken.id,
            tokenTimeout: 600,
            largeFileKiB: 1,
          },
          workspace: tokenWorkspace,
          idempotencyKey: `token-run:start:${suffix}:${marker}`,
        },
        { signal: AbortSignal.timeout(30_000) },
      )
      externalTokenRuns.push(run)
    }
    await Promise.all(
      externalTokenRuns.map((run) =>
        waitForRunStatus(client, run.id, ["waiting"]),
      ),
    )
    const completed = await client.tokens.complete(
      completedToken.id,
      {
        result: { approved: true, marker },
        idempotencyKey: `token:complete:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    assertEqual(completed.status, "completed", "external Token did not complete")
    const fanoutResults = await Promise.all(
      externalTokenRuns.map((run) =>
        client.runs.wait(
          run,
          { signal: AbortSignal.timeout(10 * 60_000) },
        ).unwrap(),
      ),
    )
    assert(
      fanoutResults.every(approvedTokenOutput),
      "external Token completion did not resume every waiting Run",
    )

    const lateWorkspace = await client.sandboxes.createWorkspace(
      "helmr-runtime-smoke",
      {
        key: `completion-before-wait-${marker}`,
        idempotencyKey: `token-workspace:create:late:${marker}`,
      },
      { signal: AbortSignal.timeout(10 * 60_000) },
    )
    workspaces.push({
      ref: lateWorkspace,
      deleteKey: `token-workspace:delete:late:${marker}`,
    })
    externalTokenWorkspaces.push(lateWorkspace.id)
    const lateRun = await client.tasks.start<typeof runtimeSmoke>(
      "runtime-smoke",
      {
        payload: {
          scenario: "completion-before-wait",
          marker: `${marker}-completion-before-wait`,
          expectedEnvironment: "unknown",
          exerciseToken: true,
          externalTokenId: completedToken.id,
          tokenTimeout: 600,
          largeFileKiB: 1,
        },
        workspace: lateWorkspace,
        idempotencyKey: `token-run:start:late:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    externalTokenRuns.push(lateRun)
    const lateResult = await client.runs.wait(
      lateRun,
      { signal: AbortSignal.timeout(10 * 60_000) },
    ).unwrap()
    assert(
      approvedTokenOutput(lateResult),
      "completion-before-wait did not resolve from the terminal Token result",
    )
    const tokenPage = await client.tokens.list(
      { limit: 100 },
      { signal: AbortSignal.timeout(30_000) },
    )
    assert(
      tokenPage.items.some((item) => item.id === completedToken.id),
      "Token list omitted the completed Token",
    )

    const timerWorkspace = await client.sandboxes.createWorkspace(
      "helmr-timer-smoke",
      {
        key: `cancel-${marker}`,
        idempotencyKey: `timer-workspace:create:${marker}`,
      },
      { signal: AbortSignal.timeout(10 * 60_000) },
    )
    workspaces.push({
      ref: timerWorkspace,
      deleteKey: `timer-workspace:delete:${marker}`,
    })
    const cancellable = await client.tasks.start<typeof timerSmoke>(
      "timer-smoke",
      {
        payload: { marker: `cancel-${marker}`, waitFor: "2m" },
        workspace: timerWorkspace,
        idempotencyKey: `timer:start:${marker}`,
      },
      { signal: AbortSignal.timeout(30_000) },
    )
    await waitForRunStatus(client, cancellable.id, ["waiting", "running"])
    await client.runs.cancel(cancellable.id, {
      signal: AbortSignal.timeout(30_000),
    })
    const cancelled = await waitForRunStatus(client, cancellable.id, ["cancelled"])
    assertEqual(cancelled.status, "cancelled", "Run cancel did not reach terminal state")
    const runs = await client.runs.list(
      { status: "cancelled", limit: 100 },
      { signal: AbortSignal.timeout(30_000) },
    )
    assert(
      runs.items.some((run) => run.id === cancellable.id),
      "Run list omitted the cancelled Run",
    )

    succeeded = true
    return {
      deploymentId: deployment.id,
      secretId: secret.id,
      completedTokenId: completedToken.id,
      externalTokenRunIds: externalTokenRuns.map((run) => run.id),
      externalTokenWorkspaceIds: externalTokenWorkspaces,
      cancelledRunId: cancellable.id,
    }
  } finally {
    const cleanup = await Promise.allSettled(
      workspaces.toReversed().map(({ ref, deleteKey }) =>
        ref.delete({ idempotencyKey: deleteKey })
      ),
    )
    if (succeeded) {
      const failure = cleanup.find((result) => result.status === "rejected")
      if (failure?.status === "rejected") throw failure.reason
    }
  }
}

function approvedTokenOutput(value: import("@helmr/sdk").JsonValue): boolean {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return false
  }
  const token = (value as Readonly<Record<string, import("@helmr/sdk").JsonValue>>)["token"]
  if (token === null || typeof token !== "object" || Array.isArray(token)) {
    return false
  }
  return (token as Readonly<Record<string, import("@helmr/sdk").JsonValue>>)["approved"] === true
}

async function waitForRunStatus(
  client: HelmrClient,
  runId: string,
  accepted: readonly Run["status"][],
): Promise<Run> {
  const deadline = Date.now() + 5 * 60_000
  for (;;) {
    const run = await client.runs.retrieve(runId, {
      signal: AbortSignal.timeout(30_000),
    })
    if (accepted.includes(run.status)) return run
    if (
      run.status === "succeeded" ||
      run.status === "failed" ||
      run.status === "cancelled" ||
      run.status === "expired" ||
      run.status === "system_failed"
    ) {
      throw new Error(`Run ${runId} reached unexpected terminal status ${run.status}`)
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for Run ${runId}: ${accepted.join(",")}`)
    }
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
}
